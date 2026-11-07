package torrentgen

import (
	"crypto/sha1"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateAndLoad(t *testing.T) {
	dir := t.TempDir()
	tor, err := Generate(dir, Spec{Name: "set", PieceLength: 32768, Announce: "http://127.0.0.1:1/announce", Seed: 7,
		Files: []File{{Path: []string{"a.bin"}, Size: 70000}, {Path: []string{"z", "empty"}, Size: 0}, {Path: []string{"z", "b.bin"}, Size: 5000}}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < tor.NumPieces(); i++ {
		data, err := tor.Piece(i)
		if err != nil || sha1.Sum(data) != tor.Pieces[i] {
			t.Fatalf("piece %d: %v", i, err)
		}
	}
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "x.torrent")
	os.WriteFile(p, tor.Bytes, 0o644)
	again, err := Load(p, dir, "http://other/announce")
	if err != nil || again.InfoHash != tor.InfoHash || again.Total != tor.Total || len(again.Files) != 3 {
		t.Fatalf("Load = %+v, %v", again, err)
	}
	if out := os.Getenv("TORRENTGEN_OUT"); out != "" { // used to cross-check with the oracle
		os.WriteFile(filepath.Join(out, "gen.torrent"), tor.Bytes, 0o644)
		os.WriteFile(filepath.Join(out, "gen.id"), []byte(tor.ID()), 0o644)
	}
}

func TestLoadHybridFixture(t *testing.T) {
	tor, err := Load("../../../fixtures/download/hybrid/hybrid.torrent", "../../../fixtures/download/hybrid/content", "http://x/announce")
	if err != nil {
		t.Fatal(err)
	}
	pads := 0
	for _, f := range tor.Files {
		if f.Pad {
			pads++
		}
	}
	if tor.ID() != "e03fc660e46a7b3bc505d0af1b1a8eecacd14a50" || pads != 4 {
		t.Fatalf("id %s pads %d", tor.ID(), pads)
	}
	for i := 0; i < tor.NumPieces(); i++ {
		data, err := tor.Piece(i)
		if err != nil || sha1.Sum(data) != tor.Pieces[i] {
			t.Fatalf("hybrid piece %d does not verify (pad handling?): %v", i, err)
		}
	}
}
