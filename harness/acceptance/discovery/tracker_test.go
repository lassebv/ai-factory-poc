package discovery

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/fakes"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

func announcesFor(tr *fakes.Tracker, tor *torrentgen.Torrent) []fakes.Announce {
	var out []fakes.Announce
	for _, a := range tr.Announces() {
		if a.InfoHash == ihStr(tor) {
			out = append(out, a)
		}
	}
	return out
}

// MT-01: BEP 12 within a tier. Eight torrents share the tiers
// [[F1 F2 F3 OK] [C]], where F1–F3 answer HTTP 503 and OK works with
// interval=10 s. Each torrent tries tier 0 in a shuffled order (the first
// tracker tried is not the same for all eight), promotes OK once it responds
// (no failing tracker is contacted again after OK's first success, across
// at least two announces), and never falls back to tier 1 while tier 0 works.
func TestTierOrderShuffleAndPromotion(t *testing.T) {
	t.Parallel()
	var fail []*fakes.Tracker
	for i := 0; i < 3; i++ {
		f := httpTracker(t)
		f.SetHTTPStatus(503)
		fail = append(fail, f)
	}
	ok := httpTracker(t)
	ok.SetInterval(10, 5)
	c := httpTracker(t)
	tiers := [][]string{{fail[0].URL(), fail[1].URL(), fail[2].URL(), ok.URL()}, {c.URL()}}
	e := newEnv(t, opts{})
	var tors []*torrentgen.Torrent
	for i := 0; i < 8; i++ {
		tor := gen(t, fmt.Sprintf("tiers-%d", i), torrentgen.Spec{Announce: fail[0].URL(), AnnounceList: tiers, Files: []torrentgen.File{{Size: 40_000}}})
		e.add(t, tor, "")
		tors = append(tors, tor)
	}
	for _, tor := range tors {
		tor := tor
		if !eventually(60*time.Second, func() bool { return len(announcesFor(ok, tor)) >= 3 }) {
			fatalf(t, e, "torrent %s: the working tier-0 tracker (interval=10) got %d announces in 60 s, want 3", tor.Name, len(announcesFor(ok, tor)))
		}
	}
	firsts := map[string]bool{}
	failedFirst := 0
	for _, tor := range tors {
		okAnn := announcesFor(ok, tor)
		firstOK := okAnn[0].Time
		first, firstName := firstOK, "OK"
		for i, f := range fail {
			for _, a := range announcesFor(f, tor) {
				if a.Time.Before(first) {
					first, firstName = a.Time, fmt.Sprintf("F%d", i+1)
				}
				if a.Time.After(firstOK) {
					t.Errorf("torrent %s: failing tracker F%d was contacted %v after OK responded: a responding tracker must be promoted to the front of its tier (BEP 12)", tor.Name, i+1, a.Time.Sub(firstOK))
				}
			}
		}
		firsts[firstName] = true
		if firstName != "OK" {
			failedFirst++
		}
		if n := len(announcesFor(c, tor)); n > 0 {
			t.Errorf("torrent %s: tier-1 tracker got %d announces although tier 0 has a working tracker", tor.Name, n)
		}
	}
	t.Logf("first tracker tried per torrent: %v; %d of 8 started with a failing one", firsts, failedFirst)
	if len(firsts) < 2 {
		t.Errorf("all 8 torrents tried the same tier-0 tracker first (%v): trackers must be shuffled within a tier (BEP 12, A4)", firsts)
	}
	if failedFirst == 0 {
		t.Errorf("no torrent tried a failing tracker before OK, so promotion was not exercised (expected ~6 of 8 with a shuffle)")
	}
}

// MT-02: when every tracker in tier 0 fails, tier 1 is tried, but only
// after both tier-0 trackers were tried; the torrent downloads from the
// tier-1 tracker's peer.
func TestNextTierAfterTierFails(t *testing.T) {
	t.Parallel()
	f1, f2 := httpTracker(t), httpTracker(t)
	f1.SetHTTPStatus(503)
	f2.SetHTTPStatus(500)
	good := httpTracker(t)
	e := newEnv(t, opts{})
	tor := gen(t, "next-tier", torrentgen.Spec{Announce: f1.URL(), AnnounceList: [][]string{{f1.URL(), f2.URL()}, {good.URL()}}})
	seeder := peer(t, tor, fakes.PeerOptions{})
	good.SetPeers(seeder.Addr())
	dir := filepath.Join(t.TempDir(), "dl")
	e.add(t, tor, dir)
	e.waitSeeding(t, tor.ID(), 60*time.Second)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
	g := announcesFor(good, tor)
	a1, a2 := announcesFor(f1, tor), announcesFor(f2, tor)
	if len(g) == 0 || len(a1) == 0 || len(a2) == 0 {
		t.Fatalf("announces: F1=%d F2=%d tier1=%d; want all tier-0 trackers tried, then tier 1", len(a1), len(a2), len(g))
	}
	if g[0].Time.Before(a1[0].Time) || g[0].Time.Before(a2[0].Time) {
		t.Errorf("tier 1 was contacted before every tier-0 tracker had been tried")
	}
}

// waitSwarm waits until swarmSeeders/swarmLeechers have the given values.
func (e *env) waitSwarm(t *testing.T, id string, seeders, leechers int, timeout time.Duration) {
	t.Helper()
	e.waitFor(t, id, timeout, fmt.Sprintf("swarmSeeders=%d swarmLeechers=%d (D14)", seeders, leechers), func(x torrent) bool {
		return x.SwarmSeeders != nil && x.SwarmLeechers != nil && *x.SwarmSeeders == seeders && *x.SwarmLeechers == leechers
	})
}

// SCR-01: HTTP scrape (BEP 48). After the first successful announce,
// torrentd scrapes /scrape (derived from /announce) with the torrent's
// info_hash, exactly once in the following 20 s (the next one is due in 30
// min), and reports the scraped counts as swarmSeeders/swarmLeechers (the
// announce said 1/0, the scrape says 40/17).
func TestHTTPScrape(t *testing.T) {
	t.Parallel()
	tr := httpTracker(t)
	tr.SetSwarm(1, 0)
	tr.SetScrape(40, 100, 17)
	e := newEnv(t, opts{})
	tor := gen(t, "http-scrape", torrentgen.Spec{Announce: tr.URL()})
	e.add(t, tor, "")
	if _, ok := tr.WaitFor(20*time.Second, func(a fakes.Announce) bool { return a.InfoHash == ihStr(tor) }); !ok {
		fatalf(t, e, "no announce within 20 s")
	}
	if !eventually(30*time.Second, func() bool { return len(tr.Scrapes()) > 0 }) {
		fatalf(t, e, "no HTTP scrape within 30 s of the first announce (A10)")
	}
	sc := tr.Scrapes()[0]
	if sc.Path != "/scrape" {
		t.Errorf("scrape path %q, want /scrape (BEP 48: replace the last \"announce\")", sc.Path)
	}
	if len(sc.InfoHashes) != 1 || sc.InfoHashes[0] != ihStr(tor) {
		t.Errorf("scrape info_hash parameters %x, want exactly the torrent's", sc.InfoHashes)
	}
	if first := announcesFor(tr, tor)[0]; sc.Time.Before(first.Time) {
		t.Errorf("scrape came before the first announce")
	}
	e.waitSwarm(t, tor.ID(), 40, 17, 15*time.Second)
	time.Sleep(20 * time.Second)
	if n := len(tr.Scrapes()); n != 1 {
		t.Errorf("%d scrapes within ~20 s, want 1 (then every 30 min)", n)
	}
}

// SCR-02: UDP scrape (BEP 15). After the first announce torrentd scrapes the
// UDP tracker with the torrent's info-hash and a valid connection id, and
// reports its counts (announce 1/0, scrape 40/17).
func TestUDPScrape(t *testing.T) {
	t.Parallel()
	tr := udpTracker(t)
	tr.SetSwarm(1, 0)
	tr.SetScrape(40, 100, 17)
	e := newEnv(t, opts{})
	tor := gen(t, "udp-scrape", torrentgen.Spec{Announce: tr.URL()})
	e.add(t, tor, "")
	ann, ok := tr.WaitFor(20*time.Second, isAnnounce(fakes.UDPEventStarted))
	if !ok {
		fatalf(t, e, "no UDP announce within 20 s")
	}
	sc, ok := tr.WaitFor(30*time.Second, func(p fakes.UDPPacket) bool { return p.Action == fakes.UDPScrape })
	if !ok {
		fatalf(t, e, "no UDP scrape within 30 s of the first announce (A10)")
	}
	if len(sc.InfoHashes) != 1 || sc.InfoHashes[0] != tor.InfoHash || !sc.KnownConn || sc.Time.Before(ann.Time) {
		t.Errorf("scrape %+v: want one info-hash (the torrent's), a valid connection id, after the announce", sc)
	}
	e.waitSwarm(t, tor.ID(), 40, 17, 15*time.Second)
}

// SCR-03: no scrape when the scrape URL can't be derived (BEP 48: the last
// path component doesn't start with "announce"); the swarm counts then come
// from the announce response. A control torrent on a /announce tracker in
// the same instance is scraped.
func TestNoScrapeWhenNotDerivable(t *testing.T) {
	t.Parallel()
	odd := httpTracker(t)
	odd.SetSwarm(9, 4)
	ctl := httpTracker(t)
	e := newEnv(t, opts{})
	tor := gen(t, "no-scrape", torrentgen.Spec{Announce: odd.URLPath("/x/tracker.php")})
	control := gen(t, "scrape-control", torrentgen.Spec{Announce: ctl.URL()})
	e.add(t, tor, "")
	e.add(t, control, "")
	if !eventually(30*time.Second, func() bool { return len(ctl.Scrapes()) > 0 }) {
		fatalf(t, e, "control: the torrent on a /announce tracker was not scraped within 30 s (A10)")
	}
	e.waitSwarm(t, tor.ID(), 9, 4, 15*time.Second)
	time.Sleep(5 * time.Second)
	if sc := odd.Scrapes(); len(sc) > 0 {
		t.Errorf("tracker /x/tracker.php was scraped at %q: its scrape URL can't be derived (BEP 48), so there must be no scrape", sc[0].Path)
	}
	for _, a := range odd.Announces() {
		if a.Path != "/x/tracker.php" {
			t.Errorf("request to %q on the tracker whose only URL is /x/tracker.php", a.Path)
		}
	}
}
