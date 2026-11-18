package discovery

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/fakes"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

// sourcesSetup has one fake peer per discovery source for two torrents:
// A (a .torrent on an HTTP tracker) and B (a magnet with a udp:// tr and an
// x.pe peer that serves the metadata). No discovered peer has pieces, so
// the torrents keep looking for peers.
type sourcesSetup struct {
	e     *env
	a, b  *torrentgen.Torrent
	peers map[string]*fakes.ExtPeer // "A/tracker", "A/dht", … ; want source = the part after "/"
	tors  map[string]*torrentgen.Torrent
}

func newSourcesSetup(t *testing.T) *sourcesSetup {
	t.Helper()
	httpTr, udpTr := httpTracker(t), udpTracker(t)
	a := gen(t, "src-a", torrentgen.Spec{Announce: httpTr.URL()})
	b := gen(t, "src-b", torrentgen.Spec{Announce: deadTracker})
	s := &sourcesSetup{a: a, b: b, peers: map[string]*fakes.ExtPeer{}, tors: map[string]*torrentgen.Torrent{}}
	mk := func(key string, tor *torrentgen.Torrent, o fakes.ExtPeerOptions) *fakes.ExtPeer {
		o.HasNone = true
		o.PEX = true
		p := extPeer(t, tor, o)
		s.peers[key], s.tors[key] = p, tor
		return p
	}
	for _, x := range []struct {
		name string
		tor  *torrentgen.Torrent
	}{{"A", a}, {"B", b}} {
		if x.name == "A" {
			pex := mk("A/pex", a, fakes.ExtPeerOptions{})
			intro := fakes.ExtPeerOptions{SendPEX: []fakes.PEXEntry{{Addr: pex.Addr(), Flags: 0x10}}}
			httpTr.SetPeers(mk("A/tracker", a, intro).Addr())
		} else {
			// No PEX source for the magnet: ADR-0007 lets torrentd leave PEX
			// unused while a torrent is in metadata.
			udpTr.SetPeers(mk("B/tracker", b, fakes.ExtPeerOptions{}).Addr())
			mk("B/magnet", b, fakes.ExtPeerOptions{ServeMetadata: true})
		}
		mk(x.name+"/dht", x.tor, fakes.ExtPeerOptions{})
		mk(x.name+"/lsd", x.tor, fakes.ExtPeerOptions{})
		mk(x.name+"/incoming", x.tor, fakes.ExtPeerOptions{})
	}
	boot := dhtNode(t, fakes.RandomID(rand.New(rand.NewPCG(201, 202))))
	boot.SetValues(a.InfoHash, s.peers["A/dht"].Addr())
	boot.SetValues(b.InfoHash, s.peers["B/dht"].Addr())
	s.e = newEnv(t, opts{Bootstrap: boot.Addr().String()})
	listener := lsd(t, s.e.inst.LSDGroup)

	s.e.add(t, a, "")
	s.e.addMagnet(t, b.Magnet([]string{udpTr.URL()}, []string{s.peers["B/magnet"].Addr().String()}), "")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && len(s.missing()) > 0 {
		for _, k := range []string{"A/incoming", "B/incoming"} {
			if !connected(s.peers[k].Stats(), s.tors[k]) && s.peers[k].Stats().Open == 0 {
				s.peers[k].Dial(s.e.peerAddr().String())
			}
		}
		listener.Send(int(s.peers["A/lsd"].Addr().Port()), "harness-lsd", a.ID())
		listener.Send(int(s.peers["B/lsd"].Addr().Port()), "harness-lsd", b.ID())
		time.Sleep(2 * time.Second)
	}
	return s
}

// missing lists the discovery peers torrentd has not handshaken with.
func (s *sourcesSetup) missing() []string {
	var out []string
	for k, p := range s.peers {
		if !connected(p.Stats(), s.tors[k]) {
			out = append(out, k)
		}
	}
	return out
}

// AC-09: peers are discovered through every source. For a .torrent on an
// HTTP tracker and a magnet with a UDP tracker, torrentd connects, within
// 60 s, to the peer each source names: HTTP tracker (A), UDP tracker (B),
// DHT get_peers (both), a PEX message from A's tracker peer, an LSD announce
// (both), and B's x.pe. It also accepts an incoming connection for each.
func TestDiscoverySources(t *testing.T) {
	t.Parallel()
	s := newSourcesSetup(t)
	if m := s.missing(); len(m) > 0 {
		fatalf(t, s.e, "after 60 s torrentd had not connected to the peers from: %v", m)
	}
}

// SRC-01 (M6): each connected peer's `source` in GET /api/v1/torrents/{id}
// (TorrentDetail.peers) is the source that found it (D11): tracker, dht,
// pex, lsd, magnet (x.pe) or incoming.
func TestPeerSourcesReported(t *testing.T) {
	t.Parallel()
	s := newSourcesSetup(t)
	if m := s.missing(); len(m) > 0 {
		fatalf(t, s.e, "after 60 s torrentd had not connected to the peers from: %v", m)
	}
	for _, tor := range []*torrentgen.Torrent{s.a, s.b} {
		r := s.e.inst.Do(t, "GET", "/api/v1/torrents/"+tor.ID(), nil, nil)
		if err := spec.ValidateJSON("TorrentDetail", r.Body); err != nil {
			t.Fatalf("GET /api/v1/torrents/%s: %v", tor.ID(), err)
		}
		got := decode(t, r.Body)
		bySource := map[string]map[string]bool{}
		for _, p := range got.Peers {
			if bySource[p.Source] == nil {
				bySource[p.Source] = map[string]bool{}
			}
			bySource[p.Source][p.Addr] = true
		}
		for k, p := range s.peers {
			if s.tors[k] != tor {
				continue
			}
			src := k[2:]
			addr := p.Addr().String()
			if src == "incoming" {
				if len(bySource["incoming"]) == 0 {
					t.Errorf("%s: no peer with source incoming; peers %+v", tor.Name, got.Peers)
				}
				continue
			}
			if !bySource[src][addr] {
				t.Errorf("%s: peer %s should be listed with source %q; peers %+v", tor.Name, addr, src, got.Peers)
			}
		}
	}
}

var _ = fmt.Sprint
