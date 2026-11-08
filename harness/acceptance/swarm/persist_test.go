package swarm

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"torrentd.local/harness/acceptance/internal/fakes"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

// waitListed waits until the torrent is listed after a restart.
func (e *env) waitListed(t *testing.T, id string, timeout time.Duration) torrent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r := e.inst.Do(t, "GET", "/api/v1/torrents/"+id, nil, nil)
		if r.Status == 200 {
			var got torrent
			json.Unmarshal(r.Body, &got)
			return got
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("torrent %s is not listed %s after restart", id, timeout)
	return torrent{}
}

// waitAnyState polls until the torrent is in one of states (error allowed).
func (e *env) waitAnyState(t *testing.T, id string, timeout time.Duration, states ...string) torrent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last torrent
	for time.Now().Before(deadline) {
		last = e.get(t, id)
		for _, s := range states {
			if last.State == s {
				return last
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("torrent did not reach %v within %s (last %s, progress %.3f)", states, timeout, last.State, last.Progress)
	return last
}

// corruptKeepingStat flips one byte in a file and restores its size and mtime.
func corruptKeepingStat(t *testing.T, path string, offset int64, keepMtime bool) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1)
	f.ReadAt(b, offset)
	b[0] ^= 0xff
	f.WriteAt(b, offset)
	f.Close()
	if keepMtime {
		os.Chtimes(path, st.ModTime(), st.ModTime())
	} else {
		later := st.ModTime().Add(10 * time.Second)
		os.Chtimes(path, later, later)
	}
}

func (r *restartable) downloadOnce(t *testing.T, name string, size int64) (*torrentgen.Torrent, string) {
	t.Helper()
	tor := r.gen(t, name, 32768, f(size, "data.bin"))
	p := r.peer(t, tor, fakes.PeerOptions{})
	r.tracker.SetPeers(p.Addr())
	_, dir := r.download(t, tor, 60*time.Second)
	return tor, dir
}

// RES-01 (§4.1 fast resume, C3, §7.2): a clean restart restores the torrent,
// its state and its lifetime totals without a full recheck. Detected by
// corrupting a byte while keeping size and mtime: a full recheck would see
// it, fast resume must not.
func TestFastResumeAfterCleanRestart(t *testing.T) {
	r := newRestartable(t)
	tor, _ := r.downloadOnce(t, "resume.bin", 2<<20)
	before := r.get(t, tor.ID())
	r.restart(t, false)
	r.waitListed(t, tor.ID(), 10*time.Second)
	got := r.waitAnyState(t, tor.ID(), 15*time.Second, "seeding", "downloading", "error")
	if got.State != "seeding" {
		t.Fatalf("after a clean restart the torrent is %s, want seeding", got.State)
	}
	if got.DownloadedTot < before.DownloadedTot || before.DownloadedTot == 0 {
		t.Errorf("downloadedTotal %d before, %d after the restart: lifetime totals must persist", before.DownloadedTot, got.DownloadedTot)
	}
}

// RES-01b: the tamper check proper (no full recheck when size+mtime match).
func TestNoRecheckWhenFilesUnchanged(t *testing.T) {
	r := newRestartable(t)
	tor, dir := r.downloadOnce(t, "norecheck.bin", 2<<20)
	r.inst.Signal(syscallTERM)
	r.inst.Wait(15 * time.Second)
	corruptKeepingStat(t, filepath.Join(dir, "norecheck.bin", "data.bin"), 100, true)
	r.startAgain(t)
	got := r.waitAnyState(t, tor.ID(), 15*time.Second, "seeding", "downloading", "error")
	if got.State != "seeding" || got.Progress != 1 {
		t.Errorf("state %s progress %.3f: with size and mtime unchanged, fast resume must trust the saved bitfield (no full recheck)", got.State, got.Progress)
	}
}

// RES-02 (§4.1): a changed mtime triggers a recheck; the damaged piece is
// found and downloaded again.
func TestRecheckWhenFileChanged(t *testing.T) {
	r := newRestartable(t)
	tor, dir := r.downloadOnce(t, "recheck.bin", 2<<20)
	r.inst.Signal(syscallTERM)
	r.inst.Wait(15 * time.Second)
	corruptKeepingStat(t, filepath.Join(dir, "recheck.bin", "data.bin"), 100, false)
	p := r.peer(t, tor, fakes.PeerOptions{})
	r.tracker.SetPeers(p.Addr())
	r.startAgain(t)
	r.waitListed(t, tor.ID(), 10*time.Second)
	r.waitState(t, tor.ID(), "seeding", 60*time.Second)
	if err := tor.Verify(dir); err != nil {
		t.Fatalf("after recheck and repair the data is wrong: %v", err)
	}
	must(t, p.Stats().BlocksSent > 0, "the damaged piece was never downloaded again: the change was not detected")
}

// RES-03: partial progress survives a restart; verified pieces are not
// downloaded again.
func TestPartialProgressSurvivesRestart(t *testing.T) {
	r := newRestartable(t)
	tor := r.gen(t, "partial.bin", 32768, f(64*32768, "data.bin"))
	p := r.peer(t, tor, fakes.PeerOptions{StallFrom: 32})
	r.tracker.SetPeers(p.Addr())
	r.mustAdd(t, tor, filepath.Join(r.home, "dl"))
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && r.get(t, tor.ID()).Progress < 0.49 {
		time.Sleep(100 * time.Millisecond)
	}
	before := r.get(t, tor.ID()).Progress
	must(t, before >= 0.49, "setup: download did not reach 50%% (%.2f)", before)
	r.inst.Signal(syscallTERM)
	r.inst.Wait(15 * time.Second)
	p2 := r.peer(t, tor, fakes.PeerOptions{})
	r.tracker.SetPeers(p2.Addr())
	r.startAgain(t)
	r.waitListed(t, tor.ID(), 10*time.Second)
	r.waitState(t, tor.ID(), "seeding", 60*time.Second)
	for _, q := range p2.Stats().Requests {
		if q.Index < 32 {
			t.Fatalf("piece %d was verified before the restart but requested again", q.Index)
		}
	}
	if err := tor.Verify(filepath.Join(r.home, "dl")); err != nil {
		t.Fatal(err)
	}
}

// RES-04 (§11 reliability): kill -9 during a download loses at most ~10 s of
// progress, never corrupts the DB, and the download completes after restart.
func TestKill9DuringDownload(t *testing.T) {
	r := newRestartable(t)
	tor := r.gen(t, "kill9.bin", 32768, f(64*32768, "data.bin"))
	p := r.peer(t, tor, fakes.PeerOptions{StallFrom: 32})
	r.tracker.SetPeers(p.Addr())
	r.mustAdd(t, tor, filepath.Join(r.home, "dl"))
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && r.get(t, tor.ID()).Progress < 0.49; {
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(12 * time.Second) // past one 10 s flush interval
	r.inst.Kill()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(r.home, "data", "torrentd.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var ok string
	db.QueryRow("PRAGMA integrity_check").Scan(&ok)
	db.Close()
	must(t, ok == "ok", "database integrity after kill -9: %q", ok)
	p2 := r.peer(t, tor, fakes.PeerOptions{})
	r.tracker.SetPeers(p2.Addr())
	r.startAgain(t)
	got := r.waitListed(t, tor.ID(), 10*time.Second)
	r.waitAnyState(t, tor.ID(), 30*time.Second, "downloading", "seeding")
	if got2 := r.get(t, tor.ID()); got2.Progress < 0.45 && got2.State != "seeding" {
		t.Errorf("after kill -9 the progress fell to %.2f (was about 0.5 for more than 10 s)", got2.Progress)
	}
	_ = got
	r.waitState(t, tor.ID(), "seeding", 60*time.Second)
	if err := tor.Verify(filepath.Join(r.home, "dl")); err != nil {
		t.Fatal(err)
	}
}

// RES-05 (§7.2 desired_state): paused and stopped torrents stay so across a restart.
func TestDesiredStatePersists(t *testing.T) {
	r := newRestartable(t)
	done, _ := r.downloadOnce(t, "stopme.bin", 200_000)
	r.mustAction(t, done.ID(), "stop-seeding")
	r.waitAnyState(t, done.ID(), 10*time.Second, "stopped")
	slow := r.gen(t, "pauseme.bin", 32768, f(2<<20, "data.bin"))
	p := r.peer(t, slow, fakes.PeerOptions{StallFrom: 4})
	r.tracker.SetPeers(p.Addr())
	r.mustAdd(t, slow, filepath.Join(r.home, "dl2"))
	r.waitAnyState(t, slow.ID(), 15*time.Second, "downloading")
	r.mustAction(t, slow.ID(), "pause")
	r.waitAnyState(t, slow.ID(), 10*time.Second, "paused")
	r.restart(t, false)
	a := r.waitListed(t, done.ID(), 10*time.Second)
	time.Sleep(2 * time.Second)
	a = r.get(t, done.ID())
	b := r.get(t, slow.ID())
	if a.State != "stopped" || a.StopReason == nil || *a.StopReason != "user" {
		t.Errorf("stopped torrent after restart: state %s stopReason %v, want stopped/user", a.State, a.StopReason)
	}
	if b.State != "paused" {
		t.Errorf("paused torrent after restart: state %s, want paused", b.State)
	}
}

// RES-06 (G8, ADR-0004): all files missing after a restart -> error "Files missing".
func TestAllFilesMissing(t *testing.T) {
	r := newRestartable(t)
	tor, dir := r.downloadOnce(t, "gone.bin", 300_000)
	r.inst.Signal(syscallTERM)
	r.inst.Wait(15 * time.Second)
	os.RemoveAll(filepath.Join(dir, "gone.bin"))
	r.startAgain(t)
	r.waitListed(t, tor.ID(), 10*time.Second)
	got := r.waitAnyState(t, tor.ID(), 20*time.Second, "error", "downloading", "seeding")
	if got.State != "error" || got.ErrorMessage == nil || !strings.Contains(strings.ToLower(*got.ErrorMessage), "missing") {
		t.Errorf("state %s error %v, want error with a 'Files missing' message", got.State, got.ErrorMessage)
	}
}
