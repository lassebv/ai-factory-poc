package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/session"
)

// Torrents adapts a Store to session.Store (§6.1 step 4, §7).
func (s *Store) Torrents() session.Store { return torrentStore{s} }

type torrentStore struct{ s *Store }

var _ session.Store = torrentStore{}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func nullTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: fmtTime(t), Valid: true}
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func parseTime(s sql.NullString) time.Time {
	if !s.Valid {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return time.Time{}
	}
	return t
}

// SaveTorrents upserts the records, their files and trackers in one
// transaction.
func (ts torrentStore) SaveTorrents(ctx context.Context, recs []session.Record) (err error) {
	tx, err := ts.s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	now := ts.s.timestamp()
	for _, r := range recs {
		id := hex.EncodeToString(r.InfoHash[:])
		_, err = tx.ExecContext(ctx, `INSERT INTO torrents (info_hash, name, metainfo, private, size_bytes, piece_length,
			download_dir, desired_state, stop_reason, bitfield, resume_fingerprint, uploaded_total, downloaded_total,
			wasted_total, queue_position, added_at, completed_at, last_error, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(info_hash) DO UPDATE SET name=excluded.name, metainfo=excluded.metainfo,
			private=excluded.private, size_bytes=excluded.size_bytes, piece_length=excluded.piece_length,
			download_dir=excluded.download_dir, desired_state=excluded.desired_state,
			stop_reason=excluded.stop_reason, bitfield=excluded.bitfield,
			resume_fingerprint=excluded.resume_fingerprint, uploaded_total=excluded.uploaded_total,
			downloaded_total=excluded.downloaded_total, wasted_total=excluded.wasted_total,
			queue_position=excluded.queue_position, completed_at=excluded.completed_at,
			last_error=excluded.last_error, updated_at=excluded.updated_at`,
			id, r.Name, r.RawInfo, r.Private, r.SizeBytes, r.PieceLength, r.DownloadDir, r.DesiredState,
			nullString(string(r.StopReason)), r.Bitfield, nullString(r.Fingerprint), r.UploadedTotal,
			r.DownloadedTotal, r.WastedTotal, r.QueuePosition, fmtTime(r.AddedAt), nullTime(r.CompletedAt),
			nullString(r.LastError), now)
		if err != nil {
			return fmt.Errorf("store: save torrent %s: %w", id, err)
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM trackers WHERE info_hash = ? AND user_added = 0`, id); err != nil {
			return fmt.Errorf("store: save trackers: %w", err)
		}
		for _, tr := range r.Trackers {
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO trackers (info_hash, url, tier) VALUES (?,?,?)`, id, tr.URL, tr.Tier); err != nil {
				return fmt.Errorf("store: save trackers: %w", err)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// LoadTorrents returns every torrent with metadata, by queue position.
func (ts torrentStore) LoadTorrents(ctx context.Context) ([]session.Record, error) {
	rows, err := ts.s.db.QueryContext(ctx, `SELECT info_hash, name, metainfo, private, COALESCE(size_bytes,0),
		COALESCE(piece_length,0), download_dir, desired_state, stop_reason, bitfield, resume_fingerprint,
		uploaded_total, downloaded_total, wasted_total, queue_position, added_at, completed_at, last_error
		FROM torrents WHERE metainfo IS NOT NULL ORDER BY queue_position, added_at`)
	if err != nil {
		return nil, fmt.Errorf("store: load torrents: %w", err)
	}
	defer rows.Close()
	var out []session.Record
	for rows.Next() {
		var r session.Record
		var id, added string
		var stop, fp, completed, lastErr sql.NullString
		if err := rows.Scan(&id, &r.Name, &r.RawInfo, &r.Private, &r.SizeBytes, &r.PieceLength, &r.DownloadDir,
			&r.DesiredState, &stop, &r.Bitfield, &fp, &r.UploadedTotal, &r.DownloadedTotal, &r.WastedTotal,
			&r.QueuePosition, &added, &completed, &lastErr); err != nil {
			return nil, fmt.Errorf("store: load torrents: %w", err)
		}
		b, err := hex.DecodeString(id)
		if err != nil || len(b) != 20 {
			continue
		}
		copy(r.InfoHash[:], b)
		r.StopReason = session.StopReason(stop.String)
		r.Fingerprint, r.LastError = fp.String, lastErr.String
		r.AddedAt = parseTime(sql.NullString{String: added, Valid: true})
		r.CompletedAt = parseTime(completed)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load torrents: %w", err)
	}
	for i := range out {
		trs, err := ts.trackers(ctx, hex.EncodeToString(out[i].InfoHash[:]))
		if err != nil {
			return nil, err
		}
		out[i].Trackers = trs
	}
	return out, nil
}

func (ts torrentStore) trackers(ctx context.Context, id string) ([]metainfo.Tracker, error) {
	rows, err := ts.s.db.QueryContext(ctx, `SELECT url, tier FROM trackers WHERE info_hash = ? ORDER BY tier, rowid`, id)
	if err != nil {
		return nil, fmt.Errorf("store: load trackers: %w", err)
	}
	defer rows.Close()
	var out []metainfo.Tracker
	for rows.Next() {
		var tr metainfo.Tracker
		if err := rows.Scan(&tr.URL, &tr.Tier); err != nil {
			return nil, fmt.Errorf("store: load trackers: %w", err)
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// SaveBan inserts or replaces a ban (ADR-0006).
func (ts torrentStore) SaveBan(ctx context.Context, b session.Ban) error {
	_, err := ts.s.db.ExecContext(ctx, `INSERT OR REPLACE INTO banned_peers (ip, reason, banned_at, expires_at) VALUES (?,?,?,?)`,
		b.IP.Unmap().String(), b.Reason, fmtTime(b.BannedAt), fmtTime(b.ExpiresAt))
	if err != nil {
		return fmt.Errorf("store: save ban: %w", err)
	}
	return nil
}

// LoadBans returns the bans still in force at now.
func (ts torrentStore) LoadBans(ctx context.Context, now time.Time) ([]session.Ban, error) {
	rows, err := ts.s.db.QueryContext(ctx, `SELECT ip, reason, banned_at, expires_at FROM banned_peers`)
	if err != nil {
		return nil, fmt.Errorf("store: load bans: %w", err)
	}
	defer rows.Close()
	var out []session.Ban
	for rows.Next() {
		var ip, reason, at, exp string
		if err := rows.Scan(&ip, &reason, &at, &exp); err != nil {
			return nil, fmt.Errorf("store: load bans: %w", err)
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(ip))
		if err != nil {
			continue
		}
		b := session.Ban{IP: addr, Reason: reason, BannedAt: parseTime(sql.NullString{String: at, Valid: true}),
			ExpiresAt: parseTime(sql.NullString{String: exp, Valid: true})}
		if b.ExpiresAt.After(now) {
			out = append(out, b)
		}
	}
	return out, rows.Err()
}
