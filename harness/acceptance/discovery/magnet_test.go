package discovery

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/fakes"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

// bigInfo is a torrent whose info dict is ~45 KB: 3 ut_metadata pieces.
func bigInfo(t *testing.T, name string, s torrentgen.Spec) *torrentgen.Torrent {
	t.Helper()
	for i := 0; i < 400; i++ {
		s.Files = append(s.Files, torrentgen.File{Path: []string{
			fmt.Sprintf("a-rather-long-directory-name-%03d", i/50),
			fmt.Sprintf("a-file-with-a-long-name-for-metadata-size-%03d.bin", i)}, Size: 1000 + int64(i)})
	}
	s.PieceLength = 16384
	tor := gen(t, name, s)
	info, _ := tor.InfoBytes()
	if len(info) <= 2*16384 {
		t.Fatalf("info dict only %d bytes; want > 32 KiB (3 metadata pieces)", len(info))
	}
	return tor
}

// requireMetadataState checks the ADR-0020 §3 shape of a fresh magnet.
func requireMetadataState(t *testing.T, got torrent, tor *torrentgen.Torrent) {
	t.Helper()
	if got.ID != tor.ID() {
		t.Errorf("id %s, want the info-hash %s", got.ID, tor.ID())
	}
	if got.State != "metadata" || got.HasMetadata || got.SizeBytes != nil {
		t.Errorf("new magnet: state=%s hasMetadata=%v sizeBytes=%v, want metadata/false/null (ADR-0020 §3, G19)", got.State, got.HasMetadata, got.SizeBytes)
	}
}

// MAG-03: a magnet (no dn, no tr) with one x.pe peer that serves
// ut_metadata: POST returns 201 in state metadata with hasMetadata=false;
// torrentd fetches all 3 metadata pieces (16 KiB each, BEP 9), verifies
// them, then GET shows the name, size and all 400 files, and the download
// completes from the same peer with correct data.
func TestMagnetMetadataFetch(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{})
	tor := bigInfo(t, "magnet-fetch", torrentgen.Spec{})
	p := extPeer(t, tor, fakes.ExtPeerOptions{ServeMetadata: true})
	dir := filepath.Join(t.TempDir(), "dl")
	got := e.addMagnet(t, tor.Magnet(nil, []string{p.Addr().String()}), dir)
	requireMetadataState(t, got, tor)

	meta := e.waitFor(t, tor.ID(), 30*time.Second, "hasMetadata=true", func(x torrent) bool { return x.HasMetadata })
	if meta.Name != tor.Name || meta.SizeBytes == nil || *meta.SizeBytes != tor.Total {
		t.Errorf("after metadata: name=%q sizeBytes=%v, want %q and %d", meta.Name, meta.SizeBytes, tor.Name, tor.Total)
	}
	if len(meta.Files) != len(tor.Files) {
		t.Errorf("GET shows %d files after the metadata arrived, want %d (ADR-0020 §3)", len(meta.Files), len(tor.Files))
	} else {
		for i, f := range meta.Files {
			want := path.Join(tor.Files[i].Path...)
			if f.SizeBytes != tor.Files[i].Size || (f.Path != want && f.Path != tor.Name+"/"+want) {
				t.Errorf("file %d: %q %d bytes, want %q %d", i, f.Path, f.SizeBytes, want, tor.Files[i].Size)
				break
			}
		}
	}
	e.waitSeeding(t, tor.ID(), 60*time.Second)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, i := range p.Stats().MetaRequests {
		seen[i] = true
	}
	if !seen[0] || !seen[1] || !seen[2] {
		t.Errorf("ut_metadata pieces requested: %v, want 0, 1 and 2", p.Stats().MetaRequests)
	}
	if bad := p.Stats().BadMessages; len(bad) > 0 {
		t.Errorf("protocol problems: %v", bad)
	}
}

// MAG-05: metadata is verified against the info-hash and size-capped (T6).
// The magnet's two x.pe peers are a peer serving corrupted metadata and a
// peer advertising metadata_size = 10 MiB + 1. torrentd fetches all of the
// corrupted metadata but stays in state metadata, and never requests
// metadata from the oversized peer. A good peer then connects in, and the
// magnet resolves and downloads from it.
func TestMagnetMetadataVerified(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{})
	tor := bigInfo(t, "magnet-verify", torrentgen.Spec{})
	bad := extPeer(t, tor, fakes.ExtPeerOptions{ServeMetadata: true, CorruptMetadata: true, HasNone: true})
	huge := extPeer(t, tor, fakes.ExtPeerOptions{MetadataSize: 10<<20 + 1, HasNone: true})
	dir := filepath.Join(t.TempDir(), "dl")
	got := e.addMagnet(t, tor.Magnet(nil, []string{bad.Addr().String(), huge.Addr().String()}), dir)
	requireMetadataState(t, got, tor)

	if !bad.WaitFor(30*time.Second, func(s fakes.ExtStats) bool { return s.MetaDataSent >= 3 }) {
		fatalf(t, e, "torrentd did not fetch the 3 metadata pieces from the x.pe peer within 30 s: %+v", bad.Stats())
	}
	if !huge.WaitFor(10*time.Second, func(s fakes.ExtStats) bool { return len(s.Handshakes) > 0 }) {
		fatalf(t, e, "torrentd did not connect to the second x.pe peer")
	}
	time.Sleep(5 * time.Second)
	if x := e.get(t, tor.ID()); x.HasMetadata || x.State != "metadata" {
		t.Fatalf("after corrupted metadata: state=%s hasMetadata=%v; metadata that doesn't hash to the info-hash must be rejected", x.State, x.HasMetadata)
	}
	if r := huge.Stats().MetaRequests; len(r) > 0 {
		t.Errorf("torrentd requested metadata pieces %v from a peer advertising metadata_size > 10 MiB (T6)", r)
	}

	good := extPeer(t, tor, fakes.ExtPeerOptions{ServeMetadata: true})
	if err := good.Dial(e.peerAddr().String()); err != nil {
		t.Fatal(err)
	}
	e.waitFor(t, tor.ID(), 30*time.Second, "hasMetadata=true from the good incoming peer", func(x torrent) bool { return x.HasMetadata })
	e.waitSeeding(t, tor.ID(), 60*time.Second)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
}

// MAG-04: a bitfield that arrives before the metadata is kept (T6). The
// seeder S sends its bitfield right after the handshake and has no BEP 10,
// so it can't serve metadata; the metadata comes from M (no pieces) only
// after S's bitfield is in. torrentd downloads everything from S on that
// first connection, without S re-sending the bitfield. W sends an early
// bitfield one byte too long: once the metadata shows the length is wrong,
// torrentd disconnects W and requests nothing from it.
func TestEarlyBitfield(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{})
	tor := gen(t, "early-bitfield", torrentgen.Spec{Files: []torrentgen.File{{Size: 900_000}}})
	gate := make(chan struct{})
	m := extPeer(t, tor, fakes.ExtPeerOptions{ServeMetadata: true, HasNone: true, MetadataGate: gate})
	s := extPeer(t, tor, fakes.ExtPeerOptions{NoExtensions: true, EarlyBitfield: true})
	w := extPeer(t, tor, fakes.ExtPeerOptions{NoExtensions: true, EarlyBitfield: true, BitfieldExtraBytes: 1})
	dir := filepath.Join(t.TempDir(), "dl")
	e.addMagnet(t, tor.Magnet(nil, []string{m.Addr().String(), s.Addr().String(), w.Addr().String()}), dir)
	for _, p := range []*fakes.ExtPeer{s, w} {
		if !p.WaitFor(20*time.Second, func(st fakes.ExtStats) bool { return connected(st, tor) }) {
			fatalf(t, e, "torrentd did not connect to all x.pe peers within 20 s")
		}
	}
	time.Sleep(time.Second) // the bitfields are on the wire
	if n := len(s.Stats().ClosedByRemote); n > 0 {
		t.Fatalf("torrentd closed the connection of a peer that sent its bitfield before the metadata arrived (T6: buffer it)")
	}
	close(gate)
	metaAt := time.Now()
	e.waitFor(t, tor.ID(), 30*time.Second, "hasMetadata=true", func(x torrent) bool { return x.HasMetadata })
	e.waitSeeding(t, tor.ID(), 60*time.Second)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Connections != 1 || len(st.ClosedByRemote) > 0 && st.ClosedByRemote[0].Before(metaAt) {
		t.Errorf("S: %d connections, closed by torrentd at %v: the early bitfield must be used on the first connection", st.Connections, st.ClosedByRemote)
	}
	if st.BlocksSent == 0 {
		t.Errorf("nothing was downloaded from S, the only peer with pieces")
	}
	if !w.WaitFor(15*time.Second, func(x fakes.ExtStats) bool { return len(x.ClosedByRemote) > 0 && x.Open == 0 }) {
		t.Errorf("W's early bitfield has the wrong length for the metadata, but torrentd kept the connection")
	}
	if n := w.Stats().Requests; n > 0 {
		t.Errorf("torrentd sent %d requests to W, whose bitfield is invalid", n)
	}
}

// MAG-06: G14. A .torrent added for a magnet still in metadata (its tr
// tracker has no peers) completes the metadata: POST returns 409
// already_exists with the id, the torrent leaves metadata, and the .torrent's
// tracker is merged in: torrentd announces there and downloads from its peer.
func TestTorrentFileCompletesMagnet(t *testing.T) {
	t.Parallel()
	empty, full := httpTracker(t), httpTracker(t)
	e := newEnv(t, opts{})
	tor := gen(t, "g14", torrentgen.Spec{Announce: full.URL()})
	seeder := peer(t, tor, fakes.PeerOptions{})
	full.SetPeers(seeder.Addr())
	dir := filepath.Join(t.TempDir(), "dl")
	got := e.addMagnet(t, tor.Magnet([]string{empty.URL()}, nil), dir)
	requireMetadataState(t, got, tor)
	if _, ok := empty.WaitFor(20*time.Second, func(a fakes.Announce) bool { return a.InfoHash == ihStr(tor) }); !ok {
		fatalf(t, e, "the magnet's tr tracker got no announce within 20 s")
	}
	time.Sleep(2 * time.Second)
	if x := e.get(t, tor.ID()); x.State != "metadata" {
		t.Fatalf("state %s before the .torrent was added, want metadata", x.State)
	}
	r := e.post(t, "file", tor.Bytes, dir)
	if r.Status != 409 {
		t.Fatalf("POST .torrent for a pending magnet = %d, want 409 (G14); body %.300s", r.Status, r.Body)
	}
	if err := spec.ValidateJSON("AlreadyExists", r.Body); err != nil {
		t.Errorf("409 body: %v", err)
	}
	var body struct {
		ID    string `json:"id"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.Unmarshal(r.Body, &body)
	if body.ID != tor.ID() || body.Error.Code != "already_exists" {
		t.Errorf("409 body id=%q code=%q, want %s already_exists", body.ID, body.Error.Code, tor.ID())
	}
	e.waitFor(t, tor.ID(), 15*time.Second, "hasMetadata=true from the .torrent (G14)", func(x torrent) bool { return x.HasMetadata })
	if _, ok := full.WaitFor(20*time.Second, func(a fakes.Announce) bool { return a.InfoHash == ihStr(tor) }); !ok {
		fatalf(t, e, "the .torrent's tracker was not merged in: no announce there within 20 s")
	}
	e.waitSeeding(t, tor.ID(), 60*time.Second)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
}
