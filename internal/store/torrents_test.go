package store

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/session"
)

func TestTorrentsRoundTrip(t *testing.T) {
	s, _ := openTemp(t)
	ts := s.Torrents()
	ctx := context.Background()
	added := time.Date(2026, 11, 8, 10, 0, 0, 123, time.UTC)
	r := session.Record{
		InfoHash: [20]byte{0xab, 1}, Name: "x", RawInfo: []byte("d4:name1:xe"), Private: true,
		Trackers:  []metainfo.Tracker{{URL: "http://a/announce", Tier: 0}, {URL: "http://b/announce", Tier: 1}},
		SizeBytes: 100, PieceLength: 16384, DownloadDir: "/d", DesiredState: session.DesiredStopped,
		StopReason: session.StopReasonUser, Bitfield: []byte{0x80}, Fingerprint: `[{"size":1}]`,
		UploadedTotal: 5, DownloadedTotal: 6, WastedTotal: 7, QueuePosition: 2, AddedAt: added,
	}
	if err := ts.SaveTorrents(ctx, []session.Record{r}); err != nil {
		t.Fatal(err)
	}
	r.DesiredState, r.StopReason, r.CompletedAt, r.LastError = session.DesiredActive, "", added.Add(time.Hour), "boom"
	r.Trackers = r.Trackers[:1]
	if err := ts.SaveTorrents(ctx, []session.Record{r}); err != nil {
		t.Fatal(err)
	}
	got, err := ts.LoadTorrents(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("LoadTorrents = %v, %v", got, err)
	}
	g := got[0]
	if g.InfoHash != r.InfoHash || g.DesiredState != session.DesiredActive || g.StopReason != "" || g.LastError != "boom" ||
		!g.AddedAt.Equal(added) || !g.CompletedAt.Equal(r.CompletedAt) || len(g.Trackers) != 1 || g.UploadedTotal != 5 ||
		string(g.Bitfield) != "\x80" || g.Fingerprint != r.Fingerprint || !g.Private || g.QueuePosition != 2 {
		t.Fatalf("round trip: %+v", g)
	}
	bad := r
	bad.DesiredState = "bogus"
	if err := ts.SaveTorrents(ctx, []session.Record{bad}); err == nil {
		t.Fatal("CHECK constraint must reject a bad desired_state")
	}
}

func TestBansRoundTrip(t *testing.T) {
	s, _ := openTemp(t)
	ts := s.Torrents()
	ctx := context.Background()
	now := time.Now()
	for _, b := range []session.Ban{
		{IP: netip.MustParseAddr("10.0.0.1"), Reason: "r", BannedAt: now, ExpiresAt: now.Add(time.Hour)},
		{IP: netip.MustParseAddr("::1"), Reason: "r", BannedAt: now, ExpiresAt: now.Add(-time.Hour)},
	} {
		if err := ts.SaveBan(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ts.LoadBans(ctx, now)
	if err != nil || len(got) != 1 || got[0].IP.String() != "10.0.0.1" {
		t.Fatalf("LoadBans = %v, %v", got, err)
	}
	s.Close()
	if _, err := ts.LoadBans(ctx, now); err == nil {
		t.Fatal("closed db")
	}
	if _, err := ts.LoadTorrents(ctx); err == nil {
		t.Fatal("closed db")
	}
	if err := ts.SaveTorrents(ctx, nil); err == nil {
		t.Fatal("closed db")
	}
	if err := ts.SaveBan(ctx, session.Ban{}); err == nil {
		t.Fatal("closed db")
	}
}
