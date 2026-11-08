package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/netip"
	"time"
)

// fmtTime formats t for a TEXT column (UTC, second precision).
func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// parseTime parses a TEXT time column; NULL or empty is the zero time.
func parseTime(ns sql.NullString) (time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, ns.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: bad time %q: %w", ns.String, err)
	}
	return t.UTC(), nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullBytes(b []byte) any {
	if b == nil {
		return nil
	}
	return b
}

// LoadTorrents returns every persisted torrent (see contract.go).
func (s *Store) LoadTorrents(ctx context.Context) ([]TorrentRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT info_hash, name, metainfo, private,
		COALESCE(size_bytes, 0), COALESCE(piece_length, 0), download_dir, desired_state,
		stop_reason, limit_override, bitfield, resume_fingerprint, uploaded_total,
		downloaded_total, wasted_total, seeding_seconds, queue_position, added_at,
		completed_at, last_error
		FROM torrents ORDER BY queue_position, added_at, info_hash`)
	if err != nil {
		return nil, fmt.Errorf("store: load torrents: %w", err)
	}
	defer rows.Close()
	var out []TorrentRecord
	for rows.Next() {
		var r TorrentRecord
		var ih, desired string
		var stop, fp, lastErr, completed sql.NullString
		var added sql.NullString
		var private, override int
		var raw, bf []byte
		if err := rows.Scan(&ih, &r.Name, &raw, &private, &r.SizeBytes, &r.PieceLength,
			&r.DownloadDir, &desired, &stop, &override, &bf, &fp, &r.UploadedTotal,
			&r.DownloadedTotal, &r.WastedTotal, &r.SeedingSeconds, &r.QueuePosition,
			&added, &completed, &lastErr); err != nil {
			return nil, fmt.Errorf("store: load torrents: %w", err)
		}
		h, err := hex.DecodeString(ih)
		if err != nil || len(h) != 20 {
			return nil, fmt.Errorf("store: bad info_hash %q", ih)
		}
		copy(r.InfoHash[:], h)
		r.RawInfo = raw
		r.Private, r.LimitOverride = private != 0, override != 0
		r.DesiredState = DesiredState(desired)
		r.StopReason, r.LastError = stop.String, lastErr.String
		r.Bitfield = bf
		if fp.Valid {
			r.ResumeFingerprint = []byte(fp.String)
		}
		if r.AddedAt, err = parseTime(added); err != nil {
			return nil, err
		}
		if r.CompletedAt, err = parseTime(completed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load torrents: %w", err)
	}
	rows.Close()
	for i := range out {
		if err := s.loadChildren(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) loadChildren(ctx context.Context, r *TorrentRecord) error {
	ih := hex.EncodeToString(r.InfoHash[:])
	rows, err := s.db.QueryContext(ctx, `SELECT url, tier FROM trackers WHERE info_hash = ? ORDER BY tier, rowid`, ih)
	if err != nil {
		return fmt.Errorf("store: load trackers: %w", err)
	}
	for rows.Next() {
		var t TrackerRecord
		if err := rows.Scan(&t.URL, &t.Tier); err != nil {
			rows.Close()
			return fmt.Errorf("store: load trackers: %w", err)
		}
		r.Trackers = append(r.Trackers, t)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT path, size_bytes FROM torrent_files WHERE info_hash = ? ORDER BY idx`, ih)
	if err != nil {
		return fmt.Errorf("store: load files: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var f FileRecord
		if err := rows.Scan(&f.Path, &f.SizeBytes); err != nil {
			return fmt.Errorf("store: load files: %w", err)
		}
		r.Files = append(r.Files, f)
	}
	return rows.Err()
}

// SaveTorrents upserts recs in one transaction (see contract.go).
func (s *Store) SaveTorrents(ctx context.Context, recs []TorrentRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: save torrents: %w", err)
	}
	defer tx.Rollback()
	now := s.timestamp()
	for _, r := range recs {
		if err := saveOne(ctx, tx, r, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: save torrents: %w", err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func saveOne(ctx context.Context, tx *sql.Tx, r TorrentRecord, now string) error {
	ih := hex.EncodeToString(r.InfoHash[:])
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM torrents WHERE info_hash = ?`, ih).Scan(&exists); err != nil {
		return fmt.Errorf("store: save torrent: %w", err)
	}
	var completed any
	if !r.CompletedAt.IsZero() {
		completed = fmtTime(r.CompletedAt)
	}
	var fp any
	if r.ResumeFingerprint != nil {
		fp = string(r.ResumeFingerprint)
	}
	desired := r.DesiredState
	if desired == "" {
		desired = DesiredActive
	}
	args := []any{r.Name, nullBytes(r.RawInfo), boolInt(r.Private), r.SizeBytes, r.PieceLength,
		r.DownloadDir, string(desired), nullString(r.StopReason), boolInt(r.LimitOverride),
		nullBytes(r.Bitfield), fp, r.UploadedTotal, r.DownloadedTotal, r.WastedTotal,
		r.SeedingSeconds, r.QueuePosition, fmtTime(r.AddedAt), completed,
		nullString(r.LastError), now, ih}
	if exists == 0 {
		_, err := tx.ExecContext(ctx, `INSERT INTO torrents (name, metainfo, private, size_bytes,
			piece_length, download_dir, desired_state, stop_reason, limit_override, bitfield,
			resume_fingerprint, uploaded_total, downloaded_total, wasted_total, seeding_seconds,
			queue_position, added_at, completed_at, last_error, updated_at, info_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...)
		if err != nil {
			return fmt.Errorf("store: insert torrent: %w", err)
		}
		for i, f := range r.Files {
			if _, err := tx.ExecContext(ctx, `INSERT INTO torrent_files (info_hash, idx, path, size_bytes) VALUES (?, ?, ?, ?)`,
				ih, i, f.Path, f.SizeBytes); err != nil {
				return fmt.Errorf("store: insert file: %w", err)
			}
		}
	} else {
		_, err := tx.ExecContext(ctx, `UPDATE torrents SET name = ?, metainfo = ?, private = ?,
			size_bytes = ?, piece_length = ?, download_dir = ?, desired_state = ?, stop_reason = ?,
			limit_override = ?, bitfield = ?, resume_fingerprint = ?, uploaded_total = ?,
			downloaded_total = ?, wasted_total = ?, seeding_seconds = ?, queue_position = ?,
			added_at = ?, completed_at = ?, last_error = ?, updated_at = ? WHERE info_hash = ?`, args...)
		if err != nil {
			return fmt.Errorf("store: update torrent: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM trackers WHERE info_hash = ?`, ih); err != nil {
			return fmt.Errorf("store: replace trackers: %w", err)
		}
	}
	for _, t := range r.Trackers {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO trackers (info_hash, url, tier) VALUES (?, ?, ?)`,
			ih, t.URL, t.Tier); err != nil {
			return fmt.Errorf("store: insert tracker: %w", err)
		}
	}
	return nil
}

// normIP returns the canonical text form of an IP (IPv4-mapped unmapped).
func normIP(s string) string {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap().WithZone("").String()
	}
	return s
}

// LoadBans deletes expired bans and returns the rest (see contract.go).
func (s *Store) LoadBans(ctx context.Context, now time.Time) ([]Ban, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ip, reason, banned_at, expires_at FROM banned_peers ORDER BY ip`)
	if err != nil {
		return nil, fmt.Errorf("store: load bans: %w", err)
	}
	var out []Ban
	var expired []string
	for rows.Next() {
		var b Ban
		var at, exp sql.NullString
		if err := rows.Scan(&b.IP, &b.Reason, &at, &exp); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: load bans: %w", err)
		}
		b.BannedAt, _ = parseTime(at)
		b.ExpiresAt, _ = parseTime(exp)
		if !b.ExpiresAt.After(now) {
			expired = append(expired, b.IP)
			continue
		}
		out = append(out, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("store: load bans: %w", err)
	}
	for _, ip := range expired {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM banned_peers WHERE ip = ?`, ip); err != nil {
			return nil, fmt.Errorf("store: delete ban: %w", err)
		}
	}
	return out, nil
}

// SaveBan upserts a ban by IP (see contract.go).
func (s *Store) SaveBan(ctx context.Context, b Ban) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO banned_peers (ip, reason, banned_at, expires_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(ip) DO UPDATE SET reason = excluded.reason, banned_at = excluded.banned_at, expires_at = excluded.expires_at`,
		normIP(b.IP), b.Reason, fmtTime(b.BannedAt), fmtTime(b.ExpiresAt))
	if err != nil {
		return fmt.Errorf("store: save ban: %w", err)
	}
	return nil
}
