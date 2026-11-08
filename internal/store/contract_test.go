package store

// Contract tests for the M4 persistence methods (contract.go, ADR-0019).

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"torrentd/internal/clock"
)

func sampleRecord(b byte) TorrentRecord {
	var h [20]byte
	h[0] = b
	return TorrentRecord{
		InfoHash:          h,
		Name:              "t" + string(rune('a'+b)),
		RawInfo:           []byte("d4:name1:ae"),
		Trackers:          []TrackerRecord{{URL: "http://a/announce", Tier: 0}, {URL: "http://b/announce", Tier: 1}},
		Private:           b%2 == 1,
		SizeBytes:         1000,
		PieceLength:       32768,
		Files:             []FileRecord{{Path: "dir/x.bin", SizeBytes: 600}, {Path: "dir/y.bin", SizeBytes: 400}},
		DownloadDir:       "/tmp/dl",
		DesiredState:      DesiredActive,
		Bitfield:          []byte{0x80},
		ResumeFingerprint: []byte(`[{"size":600,"mtime":1}]`),
		UploadedTotal:     11, DownloadedTotal: 22, WastedTotal: 33, SeedingSeconds: 44,
		QueuePosition: int(b),
		AddedAt:       time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func TestContractStoreTorrentRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	a, b := sampleRecord(2), sampleRecord(1)
	b.DesiredState, b.StopReason, b.LimitOverride = DesiredStopped, "user", true
	b.CompletedAt = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	b.Bitfield, b.ResumeFingerprint = nil, nil
	b.LastError = "Files missing"
	if err := s.SaveTorrents(ctx, []TorrentRecord{a, b}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadTorrents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []TorrentRecord{b, a} // queue_position order
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}

	// Update: mutable fields change, trackers are replaced, nothing else
	// disappears.
	a.Bitfield = []byte{0xc0}
	a.UploadedTotal = 99
	a.DesiredState = DesiredPaused
	a.Trackers = []TrackerRecord{{URL: "http://c/announce", Tier: 0}}
	if err := s.SaveTorrents(ctx, []TorrentRecord{a}); err != nil {
		t.Fatal(err)
	}
	got, err = s.LoadTorrents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !reflect.DeepEqual(got[1], a) || !reflect.DeepEqual(got[0], b) {
		t.Fatalf("after update:\n got %+v\nwant %+v, %+v", got, b, a)
	}
}

func TestContractStoreSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	s, err := Open(ctx, path, clock.NewFake(time.Unix(1e9, 0)))
	if err != nil {
		t.Fatal(err)
	}
	r := sampleRecord(3)
	if err := s.SaveTorrents(ctx, []TorrentRecord{r}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, path, clock.NewFake(time.Unix(1e9, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.LoadTorrents(ctx)
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], r) {
		t.Fatalf("after reopen: %+v %v", got, err)
	}
}

func TestContractStoreBans(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, b := range []Ban{
		{IP: "127.0.0.1", Reason: "hash failures", BannedAt: t0, ExpiresAt: t0.Add(24 * time.Hour)},
		{IP: "::1", Reason: "hash failures", BannedAt: t0.Add(-23 * time.Hour), ExpiresAt: t0.Add(time.Hour)},
	} {
		if err := s.SaveBan(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LoadBans(ctx, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].IP != "127.0.0.1" || !got[0].ExpiresAt.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("unexpired bans = %+v, want only 127.0.0.1", got)
	}
	// The expired row was deleted, not just filtered.
	var n int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM banned_peers`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("banned_peers rows = %d (%v), want 1 after expiry cleanup", n, err)
	}
	// Re-ban extends.
	if err := s.SaveBan(ctx, Ban{IP: "127.0.0.1", Reason: "again", BannedAt: t0.Add(time.Hour), ExpiresAt: t0.Add(25 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.LoadBans(ctx, t0.Add(24*time.Hour+30*time.Minute))
	if len(got) != 1 {
		t.Fatalf("re-ban did not extend the expiry: %+v", got)
	}
}
