// Package store is the SQLite repository layer (§6 Store, §7). All SQL lives
// here. The database runs in WAL mode with busy_timeout=5000 and
// foreign_keys=ON, and embedded migrations are applied when it is opened.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"torrentd/internal/clock"
)

// FileName is the database file name inside the data directory (§7).
const FileName = "torrentd.db"

// BusyTimeout is the SQLite busy_timeout in milliseconds (§6 Store).
const BusyTimeout = 5000

// Internal settings keys. They are never returned by or accepted from the
// /settings API (§16.6 G3, C3).
const (
	KeyCleanShutdown = "clean_shutdown"
	KeyDHTNodeIDv4   = "dht_node_id_v4"
	KeyDHTNodeIDv6   = "dht_node_id_v6"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("store: not found")

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one embedded schema migration.
type Migration struct {
	// Version is the numeric prefix of the file name (0001_init.sql → 1).
	Version int
	// Name is the file name.
	Name string
	// SQL is the file content.
	SQL string
}

// Store wraps the torrentd SQLite database.
type Store struct {
	db    *sql.DB
	clock clock.Clock
}

// Path returns the database path inside dataDir.
func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }

// DSN returns the modernc.org/sqlite data source name for path, with the
// per-connection pragmas WAL, busy_timeout and foreign_keys set.
func DSN(path string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", BusyTimeout))
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	return "file:" + path + "?" + q.Encode()
}

// Open opens (creating if needed) the database at path and applies all
// pending migrations. The clock supplies migration timestamps.
func Open(ctx context.Context, path string, clk clock.Clock) (*Store, error) {
	db, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{db: db, clock: clk}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	migs, err := Migrations()
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err := s.Migrate(ctx, migs); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// DB returns the underlying database handle.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Migrations returns the embedded migrations sorted by version.
func Migrations() ([]Migration, error) { return loadMigrations(migrationFS, "migrations") }

func loadMigrations(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var migs []Migration
	seen := map[int]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %s: name must start with a positive version and '_'", name)
		}
		if other, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %s and %s share version %d", other, name, v)
		}
		seen[v] = name
		body, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		migs = append(migs, Migration{Version: v, Name: name, SQL: string(body)})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].Version < migs[j].Version })
	return migs, nil
}

// AppliedVersions returns the versions recorded in schema_migrations, in
// ascending order. It returns nil if the table does not exist yet.
func (s *Store) AppliedVersions(ctx context.Context) ([]int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&n); err != nil {
		return nil, fmt.Errorf("check schema_migrations: %w", err)
	}
	if n == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	var vs []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		vs = append(vs, v)
	}
	return vs, rows.Err()
}

// Migrate applies every migration in migs that is not yet recorded in
// schema_migrations, each in its own transaction, and returns the versions
// it applied.
func (s *Store) Migrate(ctx context.Context, migs []Migration) ([]int, error) {
	done, err := s.AppliedVersions(ctx)
	if err != nil {
		return nil, err
	}
	have := map[int]bool{}
	for _, v := range done {
		have[v] = true
	}
	var applied []int
	for _, m := range migs {
		if have[m.Version] {
			continue
		}
		if err := s.apply(ctx, m); err != nil {
			return applied, err
		}
		applied = append(applied, m.Version)
	}
	return applied, nil
}

func (s *Store) apply(ctx context.Context, m Migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.Version, s.timestamp()); err != nil {
		return fmt.Errorf("migration %s: record version: %w", m.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %s: commit: %w", m.Name, err)
	}
	return nil
}

func (s *Store) timestamp() string { return s.clock.Now().UTC().Format(time.RFC3339) }

// Setting returns the raw (JSON-encoded) value of a settings key, or
// ErrNotFound.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read setting %s: %w", key, err)
	}
	return v, nil
}

// SetSetting stores the raw (JSON-encoded) value of a settings key.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("write setting %s: %w", key, err)
	}
	return nil
}

// DeleteSetting removes a settings key. Removing a missing key is not an error.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete setting %s: %w", key, err)
	}
	return nil
}

// BeginSession records that a session started: it clears the clean-shutdown
// marker (§16.3 C3, ADR-0002) and reports whether the previous run shut down
// cleanly. A database that has never been used counts as clean.
func (s *Store) BeginSession(ctx context.Context) (previousClean bool, err error) {
	_, err = s.Setting(ctx, KeyCleanShutdown)
	switch {
	case err == nil:
		previousClean = true
	case errors.Is(err, ErrNotFound):
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM torrents`).Scan(&n); err != nil {
			return false, fmt.Errorf("count torrents: %w", err)
		}
		previousClean = n == 0
	default:
		return false, err
	}
	if err := s.DeleteSetting(ctx, KeyCleanShutdown); err != nil {
		return false, err
	}
	return previousClean, nil
}

// MarkCleanShutdown writes the clean-shutdown marker (§16.3 C3). It is the
// last write of a graceful shutdown.
func (s *Store) MarkCleanShutdown(ctx context.Context) error {
	return s.SetSetting(ctx, KeyCleanShutdown, strconv.Quote(s.timestamp()))
}
