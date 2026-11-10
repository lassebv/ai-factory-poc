package discovery

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/fakes"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

// checkExtHandshake checks torrentd's BEP 10 handshake (ADR-0020 §4, D13).
// On incoming connections `p` may be omitted (BEP 10: the receiving side's
// port is already known) but must be right when present.
func checkExtHandshake(t *testing.T, who string, e *env, d map[string]any, metadataSize int, wantPEX bool) {
	t.Helper()
	m, _ := d["m"].(map[string]any)
	meta, _ := m["ut_metadata"].(int64)
	pex, hasPEX := m["ut_pex"].(int64)
	if meta <= 0 {
		t.Errorf("%s: m.ut_metadata = %v, want a positive id", who, m["ut_metadata"])
	}
	if wantPEX && pex <= 0 {
		t.Errorf("%s: m.ut_pex = %v, want a positive id", who, m["ut_pex"])
	}
	if !wantPEX && hasPEX && pex != 0 {
		t.Errorf("%s: m.ut_pex = %d in a handshake for a private torrent (ADR-0020 §5)", who, pex)
	}
	if wantPEX && meta == pex {
		t.Errorf("%s: ut_metadata and ut_pex share id %d", who, meta)
	}
	if v, _ := d["v"].(string); v != "torrentd 0.1.0" {
		t.Errorf("%s: v = %q, want \"torrentd 0.1.0\" (D13)", who, d["v"])
	}
	if p, ok := d["p"].(int64); (ok || who != "incoming") && int(p) != e.peerPort {
		t.Errorf("%s: p = %v, want the peer port %d", who, d["p"], e.peerPort)
	}
	if q, _ := d["reqq"].(int64); q != 250 {
		t.Errorf("%s: reqq = %v, want 250 (D1)", who, d["reqq"])
	}
	if ms, _ := d["metadata_size"].(int64); int(ms) != metadataSize {
		t.Errorf("%s: metadata_size = %v, want %d (the info dict size)", who, d["metadata_size"], metadataSize)
	}
	if y, _ := d["yourip"].(string); y != "\x7f\x00\x00\x01" {
		t.Errorf("%s: yourip = %q, want 127.0.0.1 as 4 bytes (§4.1)", who, d["yourip"])
	}
}

// EXT-01: BEP 10. torrentd sets reserved bit 20, and to every peer that
// also sets it, on outgoing and incoming connections, sends an extended
// handshake with m (ut_metadata, ut_pex), v="torrentd 0.1.0", p=peer port
// (optional on incoming connections, BEP 10), reqq=250, metadata_size and
// yourip. It uses the ids from the peer's own m
// (this fake advertises 7 and 9). A peer without bit 20 gets no extended
// messages at all.
func TestExtendedHandshake(t *testing.T) {
	t.Parallel()
	tr := httpTracker(t)
	e := newEnv(t, opts{})
	tor := gen(t, "ext-handshake", torrentgen.Spec{Announce: tr.URL()})
	info, _ := tor.InfoBytes()
	out := extPeer(t, tor, fakes.ExtPeerOptions{PEX: true, ServeMetadata: true})
	plain := extPeer(t, tor, fakes.ExtPeerOptions{NoExtensions: true})
	tr.SetPeers(out.Addr(), plain.Addr())
	e.add(t, tor, "")

	if !out.WaitFor(20*time.Second, func(s fakes.ExtStats) bool { return len(s.ExtHandshakes) > 0 }) {
		fatalf(t, e, "no extended handshake from torrentd within 20 s on an outgoing connection (stats %+v)", out.Stats())
	}
	st := out.Stats()
	if h := st.Handshakes[0]; h.Reserved[5]&0x10 == 0 {
		t.Errorf("reserved bytes %x: bit 20 (BEP 10) not set", h.Reserved)
	}
	checkExtHandshake(t, "outgoing", e, st.ExtHandshakes[0], len(info), true)

	in := extPeer(t, tor, fakes.ExtPeerOptions{PEX: true, HasNone: true})
	if err := in.Dial(e.peerAddr().String()); err != nil {
		t.Fatal(err)
	}
	if !in.WaitFor(20*time.Second, func(s fakes.ExtStats) bool { return len(s.ExtHandshakes) > 0 }) {
		fatalf(t, e, "no extended handshake within 20 s on an incoming connection")
	}
	checkExtHandshake(t, "incoming", e, in.Stats().ExtHandshakes[0], len(info), true)

	e.waitSeeding(t, tor.ID(), 60*time.Second)
	time.Sleep(2 * time.Second)
	if !connected(plain.Stats(), tor) {
		t.Errorf("torrentd never connected to the peer without BEP 10")
	}
	if n := plain.Stats().MsgCounts[20]; n > 0 {
		t.Errorf("torrentd sent %d extended messages to a peer that did not set reserved bit 20", n)
	}
	for _, p := range []*fakes.ExtPeer{out, in} {
		if bad := p.Stats().BadMessages; len(bad) > 0 {
			t.Errorf("protocol problems: %v", bad)
		}
	}
}

// PEX-01: torrentd uses peers it learns from ut_pex. The tracker returns
// only an introducer with no pieces; its ut_pex message names a seeder no
// one else knows; torrentd connects to that seeder and downloads.
func TestPEXReceive(t *testing.T) {
	t.Parallel()
	tr := httpTracker(t)
	e := newEnv(t, opts{})
	tor := gen(t, "pex-receive", torrentgen.Spec{Announce: tr.URL()})
	seeder := extPeer(t, tor, fakes.ExtPeerOptions{PEX: true})
	intro := extPeer(t, tor, fakes.ExtPeerOptions{HasNone: true, PEX: true,
		SendPEX: []fakes.PEXEntry{{Addr: seeder.Addr(), Flags: 0x12}}})
	tr.SetPeers(intro.Addr())
	dir := filepath.Join(t.TempDir(), "dl")
	e.add(t, tor, dir)
	if !intro.WaitFor(20*time.Second, func(s fakes.ExtStats) bool { return len(s.ExtHandshakes) > 0 }) {
		fatalf(t, e, "no extended handshake to the introducer within 20 s")
	}
	e.waitSeeding(t, tor.ID(), 60*time.Second)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
	if st := seeder.Stats(); !connected(st, tor) || st.BlocksSent == 0 {
		t.Errorf("the PEX-introduced seeder was not used: %+v", st)
	}
}

func pexHas(m fakes.PEXMsg, a netip.AddrPort) (fakes.PEXEntry, bool) {
	for _, x := range m.Added {
		if x.Addr == a {
			return x, true
		}
	}
	return fakes.PEXEntry{}, false
}

// PEX-02: torrentd sends ut_pex (BEP 11, D3) using the peer's own id. A, B
// and C come from the tracker; B is a seed (that never unchokes, so the
// download stays incomplete). Within 90 s A's messages add B and C at their
// listen addresses, with flag 0x02 (seed) for B and not for C. Then C
// disconnects and D
// connects in; A's next message drops C and adds D, and comes no sooner
// than one minute after the previous one.
func TestPEXSend(t *testing.T) {
	t.Parallel()
	tr := httpTracker(t)
	e := newEnv(t, opts{})
	tor := gen(t, "pex-send", torrentgen.Spec{Announce: tr.URL()})
	a := extPeer(t, tor, fakes.ExtPeerOptions{HasNone: true, PEX: true})
	b := extPeer(t, tor, fakes.ExtPeerOptions{PEX: true, Choke: true})
	c := extPeer(t, tor, fakes.ExtPeerOptions{HasNone: true, PEX: true})
	tr.SetPeers(a.Addr(), b.Addr(), c.Addr())
	e.add(t, tor, "")
	for _, p := range []*fakes.ExtPeer{a, b, c} {
		if !p.WaitFor(20*time.Second, func(s fakes.ExtStats) bool { return len(s.ExtHandshakes) > 0 }) {
			fatalf(t, e, "torrentd did not connect (with an extended handshake) to all three tracker peers within 20 s")
		}
	}
	var be, ce fakes.PEXEntry
	var okB, okC bool
	if !a.WaitFor(90*time.Second, func(s fakes.ExtStats) bool {
		for _, m := range s.PEX {
			if x, ok := pexHas(m, b.Addr()); ok && !okB {
				be, okB = x, true
			}
			if x, ok := pexHas(m, c.Addr()); ok && !okC {
				ce, okC = x, true
			}
		}
		return okB && okC
	}) {
		fatalf(t, e, "within 90 s of connecting, A's ut_pex messages did not add both B %v and C %v (D3: first message about 1 min after connecting): %+v", b.Addr(), c.Addr(), a.Stats().PEX)
	}
	nBefore := len(a.Stats().PEX)
	if be.Flags&0x02 == 0 {
		t.Errorf("B is a seed but its PEX flags are %#x (want 0x02, BEP 11)", be.Flags)
	}
	if ce.Flags&0x02 != 0 {
		t.Errorf("C has no pieces but its PEX flags are %#x (0x02 means seed)", ce.Flags)
	}

	c.Close()
	d := extPeer(t, tor, fakes.ExtPeerOptions{HasNone: true, PEX: true})
	if err := d.Dial(e.peerAddr().String()); err != nil {
		t.Fatal(err)
	}
	var droppedC, addedD bool
	a.WaitFor(150*time.Second, func(s fakes.ExtStats) bool {
		dAddrs := append([]netip.AddrPort{d.Addr()}, d.Stats().DialedFrom...)
		for _, m := range s.PEX[nBefore:] {
			for _, dr := range m.Dropped {
				droppedC = droppedC || dr == c.Addr()
			}
			for _, da := range dAddrs {
				if _, ok := pexHas(m, da); ok {
					addedD = true
				}
			}
		}
		return droppedC && addedD
	})
	msgs := a.Stats().PEX
	if !droppedC {
		fatalf(t, e, "no ut_pex to A dropped C within 150 s of C disconnecting; messages after the first: %+v", msgs[nBefore:])
	}
	if !addedD {
		t.Errorf("no ut_pex to A added D (listen %v, or its connection address) within 150 s of D connecting: %+v", d.Addr(), msgs[nBefore:])
	}
	for i := 1; i < len(msgs); i++ {
		if gap := msgs[i].Time.Sub(msgs[i-1].Time); gap < 55*time.Second {
			t.Errorf("ut_pex messages %d and %d to A only %v apart: at most one per minute (BEP 11)", i, i+1, gap)
		}
	}
	for _, m := range msgs {
		if m.Err != "" {
			t.Errorf("ut_pex: %s", m.Err)
		}
		if len(m.Added) > 50 || len(m.Dropped) > 50 {
			t.Errorf("ut_pex with %d added / %d dropped, want at most 50 each (D3)", len(m.Added), len(m.Dropped))
		}
	}
	if bad := a.Stats().BadMessages; len(bad) > 0 {
		t.Errorf("protocol problems on A: %v", bad)
	}
}

var _ = fmt.Sprint
