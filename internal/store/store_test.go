package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"torrentd/internal/clock"
)

var t0 = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	s, err := Open(context.Background(), path, clock.NewFake(t0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func pragma(t *testing.T, s *Store, name string) string {
	t.Helper()
	var v string
	if err := s.DB().QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPragmas(t *testing.T) {
	s, _ := openTemp(t)
	if v := pragma(t, s, "journal_mode"); v != "wal" {
		t.Errorf("journal_mode = %s", v)
	}
	if v := pragma(t, s, "busy_timeout"); v != "5000" {
		t.Errorf("busy_timeout = %s", v)
	}
	if v := pragma(t, s, "foreign_keys"); v != "1" {
		t.Errorf("foreign_keys = %s", v)
	}
}

func TestMigrationsApplyOnce(t *testing.T) {
	ctx := context.Background()
	s, path := openTemp(t)
	vs, err := s.AppliedVersions(ctx)
	if err != nil || len(vs) == 0 || vs[0] != 1 {
		t.Fatalf("versions %v, %v", vs, err)
	}
	var at string
	if err := s.DB().QueryRow(`SELECT applied_at FROM schema_migrations WHERE version = 1`).Scan(&at); err != nil || at != "2026-11-07T12:00:00Z" {
		t.Fatalf("applied_at %q, %v", at, err)
	}
	for _, table := range []string{"torrents", "torrent_files", "trackers", "transfer_samples", "dht_nodes", "banned_peers", "settings", "global_stats"} {
		var n int
		if err := s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing", table)
		}
	}
	s.Close()

	s2, err := Open(ctx, path, clock.NewFake(t0.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	migs, _ := Migrations()
	applied, err := s2.Migrate(ctx, migs)
	if err != nil || len(applied) != 0 {
		t.Fatalf("re-migrate applied %v, %v", applied, err)
	}
	vs2, _ := s2.AppliedVersions(ctx)
	if len(vs2) != len(vs) {
		t.Fatalf("versions changed: %v -> %v", vs, vs2)
	}
}

func TestMigrateNewVersionAndFailure(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	applied, err := s.Migrate(ctx, []Migration{{Version: 2, Name: "0002_x.sql", SQL: "CREATE TABLE x (a INTEGER);"}})
	if err != nil || len(applied) != 1 || applied[0] != 2 {
		t.Fatalf("applied %v, %v", applied, err)
	}
	_, err = s.Migrate(ctx, []Migration{{Version: 3, Name: "0003_bad.sql", SQL: "CREATE TABLE x (a INTEGER);"}})
	if err == nil || !strings.Contains(err.Error(), "0003_bad.sql") {
		t.Fatalf("bad migration: %v", err)
	}
	vs, _ := s.AppliedVersions(ctx)
	if len(vs) != 2 {
		t.Fatalf("failed migration was recorded: %v", vs)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	s, _ := openTemp(t)
	_, err := s.DB().Exec(`INSERT INTO torrent_files (info_hash, idx, path, size_bytes) VALUES ('nope', 0, 'a', 1)`)
	if err == nil {
		t.Fatal("insert with dangling foreign key succeeded")
	}
}

func TestLoadMigrations(t *testing.T) {
	good := fstest.MapFS{
		"m/0002_b.sql": {Data: []byte("b")},
		"m/0001_a.sql": {Data: []byte("a")},
		"m/README.md":  {Data: []byte("x")},
	}
	migs, err := loadMigrations(good, "m")
	if err != nil || len(migs) != 2 || migs[0].Version != 1 || migs[1].SQL != "b" {
		t.Fatalf("got %+v, %v", migs, err)
	}
	for name, fsys := range map[string]fstest.MapFS{
		"bad name":  {"m/init.sql": {Data: []byte("a")}},
		"zero":      {"m/0000_a.sql": {Data: []byte("a")}},
		"duplicate": {"m/0001_a.sql": {Data: []byte("a")}, "m/1_b.sql": {Data: []byte("b")}},
	} {
		if _, err := loadMigrations(fsys, "m"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := loadMigrations(good, "missing"); err == nil {
		t.Error("missing dir: no error")
	}
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	if _, err := s.Setting(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	if err := s.SetSetting(ctx, "k", `1`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(ctx, "k", `2`); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Setting(ctx, "k"); err != nil || v != "2" {
		t.Fatalf("got %q, %v", v, err)
	}
	if err := s.DeleteSetting(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSetting(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Setting(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted key: %v", err)
	}
}

func TestCleanShutdownMarker(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	clean, err := s.BeginSession(ctx)
	if err != nil || !clean {
		t.Fatalf("fresh db: clean=%v err=%v", clean, err)
	}
	if err := s.MarkCleanShutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if clean, err := s.BeginSession(ctx); err != nil || !clean {
		t.Fatalf("after marker: clean=%v err=%v", clean, err)
	}
	if _, err := s.DB().Exec(`INSERT INTO torrents (info_hash, name, download_dir, desired_state, queue_position, added_at, updated_at)
		VALUES ('aa', 'n', '/d', 'active', 0, 't', 't')`); err != nil {
		t.Fatal(err)
	}
	if clean, err := s.BeginSession(ctx); err != nil || clean {
		t.Fatalf("no marker with torrents: clean=%v err=%v", clean, err)
	}
}

func TestClosedErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	s.Close()
	if _, err := s.Setting(ctx, "k"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Setting on closed db: %v", err)
	}
	if err := s.SetSetting(ctx, "k", "v"); err == nil {
		t.Error("SetSetting on closed db: no error")
	}
	if err := s.DeleteSetting(ctx, "k"); err == nil {
		t.Error("DeleteSetting on closed db: no error")
	}
	if _, err := s.AppliedVersions(ctx); err == nil {
		t.Error("AppliedVersions on closed db: no error")
	}
	if _, err := s.BeginSession(ctx); err == nil {
		t.Error("BeginSession on closed db: no error")
	}
	if _, err := s.Migrate(ctx, nil); err == nil {
		t.Error("Migrate on closed db: no error")
	}
}

func TestOpenErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-dir", FileName)
	if _, err := Open(context.Background(), path, clock.NewFake(t0)); err == nil {
		t.Fatal("Open in a missing directory succeeded")
	}
}

func TestPathAndDSN(t *testing.T) {
	if got := Path("/d"); got != filepath.Join("/d", "torrentd.db") {
		t.Errorf("Path = %s", got)
	}
	dsn := DSN("/d/torrentd.db")
	for _, want := range []string{"journal_mode%28WAL%29", "busy_timeout%285000%29", "foreign_keys%281%29"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("DSN %s lacks %s", dsn, want)
		}
	}
}
