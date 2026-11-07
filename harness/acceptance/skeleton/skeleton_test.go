// Package skeleton holds black-box acceptance tests for milestone 1: the
// binary starts, serves /healthz and the SPA, applies migrations, honors its
// configuration and enforces the Host/Origin rules (spec/acceptance.yaml
// SKEL-*, SEC-*).
package skeleton

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"torrentd.local/harness/acceptance/internal/app"
	"torrentd.local/harness/acceptance/internal/oas"
)

func TestMain(m *testing.M) { os.Exit(app.Main(m)) }

var spec = func() *oas.Spec {
	s, err := oas.LoadRepo(app.RepoRoot())
	if err != nil {
		panic(err)
	}
	return s
}()

// errorCode asserts an error response and returns its code.
func errorCode(t *testing.T, r app.Response) string {
	t.Helper()
	if err := spec.ValidateJSON("Error", r.Body); err != nil {
		t.Fatalf("status %d: body is not a valid error object: %v", r.Status, err)
	}
	var e struct{ Error struct{ Code string } }
	_ = json.Unmarshal(r.Body, &e)
	return e.Error.Code
}

func TestHealthz(t *testing.T) {
	inst := app.Shared(t)
	r := inst.Do(t, "GET", "/healthz", nil, nil)
	if r.Status != 200 || strings.TrimSpace(string(r.Body)) != "ok" {
		t.Fatalf("GET /healthz = %d %q, want 200 \"ok\"", r.Status, r.Body)
	}
}

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file %s: %v", path, err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func columns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%q)", table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols[name] = true
	}
	return cols
}

// schema is §7.1 plus the §16.6 additions.
var schema = map[string][]string{
	"schema_migrations": {"version", "applied_at"},
	"torrents": {"info_hash", "name", "metainfo", "magnet_uri", "private", "size_bytes", "piece_length",
		"download_dir", "desired_state", "stop_reason", "limit_override", "bitfield", "resume_fingerprint",
		"uploaded_total", "downloaded_total", "wasted_total", "seeding_seconds", "ratio_limit",
		"seed_time_limit", "download_limit", "upload_limit", "max_peers", "queue_position", "added_at",
		"completed_at", "last_error", "updated_at"},
	"torrent_files":    {"info_hash", "idx", "path", "size_bytes", "priority"},
	"trackers":         {"info_hash", "url", "tier", "user_added"},
	"transfer_samples": {"info_hash", "ts", "download_rate", "upload_rate"},
	"dht_nodes":        {"node_id", "addr", "last_seen"},
	"banned_peers":     {"ip", "reason", "banned_at", "expires_at"},
	"settings":         {"key", "value"},
	"global_stats":     {"id", "uploaded_total", "downloaded_total", "wasted_total", "updated_at"},
}

func migrationRows(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query("SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatalf("reading schema_migrations: %v", err)
	}
	defer rows.Close()
	var vs []int
	for rows.Next() {
		var v int
		_ = rows.Scan(&v)
		vs = append(vs, v)
	}
	return vs
}

func TestMigrations(t *testing.T) {
	home := t.TempDir()
	inst := app.Start(t, app.Options{Home: home})
	dbPath := filepath.Join(inst.DataDir, "torrentd.db")
	inst.Signal(syscall.SIGTERM)
	if _, ok := inst.Wait(10 * time.Second); !ok {
		t.Fatal("did not exit after SIGTERM")
	}

	db := openDB(t, dbPath)
	for table, cols := range schema {
		have := columns(t, db, table)
		if len(have) == 0 {
			t.Errorf("table %s is missing", table)
			continue
		}
		for _, c := range cols {
			if !have[c] {
				t.Errorf("table %s is missing column %s", table, c)
			}
		}
	}
	first := migrationRows(t, db)
	if len(first) == 0 || first[0] != 1 {
		t.Fatalf("schema_migrations versions = %v, want to start at 1", first)
	}
	db.Close()

	// A restart on the same data dir applies nothing new.
	inst2 := app.Start(t, app.Options{Home: home})
	if inst2.DataDir != inst.DataDir {
		t.Fatalf("data dir changed: %s vs %s", inst2.DataDir, inst.DataDir)
	}
	inst2.Signal(syscall.SIGTERM)
	inst2.Wait(10 * time.Second)
	second := migrationRows(t, openDB(t, dbPath))
	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Fatalf("restart changed schema_migrations: %v -> %v", first, second)
	}
}

func TestWALMode(t *testing.T) {
	inst := app.Start(t, app.Options{})
	db := openDB(t, filepath.Join(inst.DataDir, "torrentd.db"))
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

func TestSPAFallback(t *testing.T) {
	inst := app.Shared(t)
	root := inst.Do(t, "GET", "/", nil, nil)
	if root.Status != 200 || !strings.HasPrefix(root.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET / = %d %s, want 200 text/html", root.Status, root.Header.Get("Content-Type"))
	}
	if !regexp.MustCompile(`(?i)<html`).Match(root.Body) {
		t.Fatalf("GET / body does not look like index.html: %.200q", root.Body)
	}
	for _, p := range []string{"/torrents/0123456789abcdef0123456789abcdef01234567", "/settings", "/some/deep/client/route"} {
		r := inst.Do(t, "GET", p, nil, nil)
		if r.Status != 200 || string(r.Body) != string(root.Body) {
			t.Errorf("GET %s = %d, want 200 with the same body as GET / (SPA fallback)", p, r.Status)
		}
	}
	for _, p := range []string{"/api/v1/does-not-exist", "/api/v1/torrents/x/unknown-action", "/api/nope"} {
		r := inst.Do(t, "GET", p, nil, nil)
		if r.Status != 404 {
			t.Errorf("GET %s = %d, want 404", p, r.Status)
			continue
		}
		if code := errorCode(t, r); code != "not_found" {
			t.Errorf("GET %s error code = %q, want not_found", p, code)
		}
	}
}

func dbExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestConfigPrecedence(t *testing.T) {
	t.Run("flag beats env", func(t *testing.T) {
		home := t.TempDir()
		flagPort, envPort := app.FreePort(t), app.FreePort(t)
		flagDir, envDir := filepath.Join(home, "flag-data"), filepath.Join(home, "env-data")
		flagAddr := fmt.Sprintf("127.0.0.1:%d", flagPort)
		app.Start(t, app.Options{
			Raw: true, Home: home, ReadyAddr: flagAddr,
			Args: []string{"--addr", flagAddr, "--data-dir", flagDir, "--peer-port", "0"},
			Env:  []string{fmt.Sprintf("TORRENTD_ADDR=127.0.0.1:%d", envPort), "TORRENTD_DATA_DIR=" + envDir},
		})
		if !dbExists(filepath.Join(flagDir, "torrentd.db")) || dbExists(filepath.Join(envDir, "torrentd.db")) {
			t.Error("--data-dir must take precedence over TORRENTD_DATA_DIR")
		}
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", envPort), time.Second); err == nil {
			c.Close()
			t.Error("something listens on the TORRENTD_ADDR port although --addr was given")
		}
	})
	t.Run("env beats default", func(t *testing.T) {
		home := t.TempDir()
		envAddr := fmt.Sprintf("127.0.0.1:%d", app.FreePort(t))
		envDir := filepath.Join(home, "env-data")
		app.Start(t, app.Options{
			Raw: true, Home: home, ReadyAddr: envAddr,
			Args: []string{"--peer-port", "0"},
			Env:  []string{"TORRENTD_ADDR=" + envAddr, "TORRENTD_DATA_DIR=" + envDir},
		})
		if !dbExists(filepath.Join(envDir, "torrentd.db")) {
			t.Error("TORRENTD_DATA_DIR was not used")
		}
	})
	t.Run("default data dir", func(t *testing.T) {
		home := t.TempDir()
		addr := fmt.Sprintf("127.0.0.1:%d", app.FreePort(t))
		var want string
		var env []string
		switch runtime.GOOS {
		case "darwin":
			want = filepath.Join(home, "Library", "Application Support", "torrentd")
		case "linux":
			xdg := filepath.Join(home, "xdg")
			env = []string{"XDG_DATA_HOME=" + xdg}
			want = filepath.Join(xdg, "torrentd")
		default:
			t.Fatalf("no default-data-dir expectation for %s", runtime.GOOS)
		}
		app.Start(t, app.Options{Raw: true, Home: home, ReadyAddr: addr, Env: env,
			Args: []string{"--addr", addr, "--peer-port", "0"}})
		if !dbExists(filepath.Join(want, "torrentd.db")) {
			t.Errorf("database not created in the default data dir %s", want)
		}
	})
}

func TestNonLoopbackWarning(t *testing.T) {
	port := app.FreePort(t)
	inst := app.Start(t, app.Options{
		Raw: true, ReadyAddr: fmt.Sprintf("127.0.0.1:%d", port),
		Args: []string{"--addr", fmt.Sprintf("0.0.0.0:%d", port), "--data-dir", t.TempDir(), "--peer-port", "0"},
	})
	warn := regexp.MustCompile(`(?m)^.*level=WARN.*(0\.0\.0\.0|\[::\]).*$`)
	if !warn.MatchString(inst.Logs()) {
		t.Fatalf("expected a level=WARN log line naming the address (0.0.0.0 or [::]); logs:\n%s", inst.Logs())
	}
}

func TestGracefulShutdown(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			inst := app.Start(t, app.Options{})
			if err := inst.Signal(sig); err != nil {
				t.Fatal(err)
			}
			code, ok := inst.Wait(10 * time.Second)
			if !ok {
				t.Fatalf("still running 10 s after %s", sig)
			}
			if code != 0 {
				t.Fatalf("exit code %d after %s, want 0; logs:\n%s", code, sig, inst.Logs())
			}
		})
	}
}

func TestHostAllowlist(t *testing.T) {
	inst := app.Shared(t)
	_, port, _ := net.SplitHostPort(inst.Addr)
	for _, host := range []string{"localhost:" + port, "127.0.0.1:" + port, "[::1]:" + port} {
		if r := inst.Do(t, "GET", "/healthz", nil, map[string]string{"Host": host}); r.Status != 200 {
			t.Errorf("Host %s: GET /healthz = %d, want 200", host, r.Status)
		}
	}
	for _, host := range []string{"evil.example:" + port, "evil.example", "localhost:1", "127.0.0.1.evil.example:" + port} {
		for _, path := range []string{"/healthz", "/", "/api/v1/torrents"} {
			r := inst.Do(t, "GET", path, nil, map[string]string{"Host": host})
			if r.Status != 403 {
				t.Errorf("Host %s: GET %s = %d, want 403", host, path, r.Status)
				continue
			}
			if code := errorCode(t, r); code != "forbidden_host" {
				t.Errorf("Host %s: code %q, want forbidden_host", host, code)
			}
		}
	}
}

func TestOriginCheck(t *testing.T) {
	inst := app.Shared(t)
	path := "/api/v1/this-route-does-not-exist"
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, origin := range []string{"http://evil.example", "null", "http://localhost:5173", "http://127.0.0.1:1"} {
			r := inst.Do(t, method, path, []byte("{}"), map[string]string{"Origin": origin, "Content-Type": "application/json"})
			if r.Status != 403 {
				t.Errorf("%s with Origin %s = %d, want 403", method, origin, r.Status)
				continue
			}
			if code := errorCode(t, r); code != "forbidden_origin" {
				t.Errorf("%s with Origin %s: code %q, want forbidden_origin", method, origin, code)
			}
		}
		same := inst.Do(t, method, path, []byte("{}"), map[string]string{"Origin": "http://" + inst.Addr, "Content-Type": "application/json"})
		none := inst.Do(t, method, path, []byte("{}"), map[string]string{"Content-Type": "application/json"})
		for name, r := range map[string]app.Response{"same-origin": same, "no Origin": none} {
			if r.Status != 404 {
				t.Errorf("%s %s to an unknown route = %d, want 404 (the check must let it through)", name, method, r.Status)
			}
		}
	}
	if r := inst.Do(t, "GET", "/healthz", nil, map[string]string{"Origin": "http://evil.example"}); r.Status != 200 {
		t.Errorf("GET with a foreign Origin = %d, want 200 (only state-changing methods are checked)", r.Status)
	}
}

func TestDevMode(t *testing.T) {
	vite := "http://localhost:5173"
	dev := app.Start(t, app.Options{Args: []string{"--dev"}})
	pre := dev.Do(t, "OPTIONS", "/api/v1/torrents", nil, map[string]string{
		"Origin": vite, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type",
	})
	if pre.Status < 200 || pre.Status > 299 {
		t.Errorf("--dev: CORS preflight = %d, want 2xx", pre.Status)
	}
	if got := pre.Header.Get("Access-Control-Allow-Origin"); got != vite {
		t.Errorf("--dev: Access-Control-Allow-Origin = %q, want %q", got, vite)
	}
	if r := dev.Do(t, "POST", "/api/v1/this-route-does-not-exist", []byte("{}"), map[string]string{"Origin": vite}); r.Status != 404 {
		t.Errorf("--dev: POST with the Vite origin = %d, want 404 (allowed through)", r.Status)
	}
	if r := dev.Do(t, "GET", "/", nil, nil); r.Status == 200 && strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") {
		t.Errorf("--dev: GET / served HTML; the embedded SPA must not be served in dev mode")
	}

	prod := app.Shared(t)
	if r := prod.Do(t, "POST", "/api/v1/this-route-does-not-exist", []byte("{}"), map[string]string{"Origin": vite}); r.Status != 403 {
		t.Errorf("without --dev: POST with the Vite origin = %d, want 403", r.Status)
	}
}
