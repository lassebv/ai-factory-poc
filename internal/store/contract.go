package store

// FROZEN CONTRACT (ADR-0019) for the milestone 4 torrent and ban
// persistence. This file is owned by the architect. The implementer provides
// the methods below on *Store in their own files (replacing stub_m4.go):
//
//	func (s *Store) LoadTorrents(ctx context.Context) ([]TorrentRecord, error)
//	func (s *Store) SaveTorrents(ctx context.Context, recs []TorrentRecord) error
//	func (s *Store) LoadBans(ctx context.Context, now time.Time) ([]Ban, error)
//	func (s *Store) SaveBan(ctx context.Context, b Ban) error
//
// # Semantics
//
//   - SaveTorrents writes every record in ONE transaction (all or nothing):
//     an upsert of the torrents row keyed by InfoHash. On first insert it
//     also writes torrent_files (Files, idx = position) and trackers
//     (Trackers); on update it rewrites every mutable column (everything in
//     TorrentRecord) and replaces the trackers rows. updated_at is set from
//     the Store clock. It never deletes torrents that are not in recs.
//   - LoadTorrents returns every torrent ordered by queue_position, then
//     added_at, then info_hash. Every field written by SaveTorrents
//     round-trips exactly (times at second precision, UTC). A NULL bitfield
//     or resume_fingerprint loads as nil; a NULL completed_at as the zero
//     time.
//   - LoadBans deletes rows whose expires_at <= now and returns the rest.
//     SaveBan upserts by IP (a re-ban extends the expiry).
//   - The database stays consistent after kill -9 at any point (WAL,
//     synchronous=NORMAL, one transaction per SaveTorrents): RES-05 checks
//     PRAGMA integrity_check.
//   - Info hashes are stored as 40 lowercase hex characters, IPs in
//     netip.Addr.String form (IPv4-mapped IPv6 addresses are unmapped first).

import "time"

// DesiredState is torrents.desired_state (§7, §5).
type DesiredState string

// Desired states.
const (
	DesiredActive  DesiredState = "active"
	DesiredPaused  DesiredState = "paused"
	DesiredStopped DesiredState = "stopped"
)

// TrackerRecord is one trackers row.
type TrackerRecord struct {
	// URL is the announce URL.
	URL string
	// Tier is the BEP 12 tier (0-based).
	Tier int
}

// FileRecord is one torrent_files row (non-pad files, in info-dict order).
type FileRecord struct {
	// Path is the file path relative to the download dir, "/"-separated.
	Path string
	// SizeBytes is the file length.
	SizeBytes int64
}

// TorrentRecord is the persisted state of one torrent (§7 torrents table).
// Columns not listed here keep their defaults at M4 (limits are M6).
type TorrentRecord struct {
	// InfoHash is the v1 info-hash.
	InfoHash [20]byte
	// Name is the torrent name.
	Name string
	// RawInfo is the raw bencoded info dict (torrents.metainfo); the
	// session rebuilds the MetaInfo with metainfo.ParseInfo.
	RawInfo []byte
	// Trackers are the announce URLs with their tiers.
	Trackers []TrackerRecord
	// Private is the BEP 27 flag.
	Private bool
	// SizeBytes is the total size of wanted files.
	SizeBytes int64
	// PieceLength is the nominal piece length.
	PieceLength int64
	// Files are the non-pad files (written on first insert only).
	Files []FileRecord
	// DownloadDir is the absolute download dir.
	DownloadDir string
	// DesiredState is what the user asked for.
	DesiredState DesiredState
	// StopReason is "", "user", "ratio_limit" or "time_limit" (NULL when "").
	StopReason string
	// LimitOverride is torrents.limit_override (§5.2, C4).
	LimitOverride bool
	// Bitfield is the verified pieces in wire format, or nil.
	Bitfield []byte
	// ResumeFingerprint is the JSON resume fingerprint, or nil. The store
	// treats it as opaque text.
	ResumeFingerprint []byte
	// Lifetime counters (payload bytes) and the cumulative seeding time.
	UploadedTotal, DownloadedTotal, WastedTotal, SeedingSeconds int64
	// QueuePosition is the global queue order (add order at M4).
	QueuePosition int
	// AddedAt is when the torrent was added.
	AddedAt time.Time
	// CompletedAt is when the torrent first reached 100%; zero if never.
	CompletedAt time.Time
	// LastError is the error message of a torrent in state error, or "".
	LastError string
}

// Ban is one banned_peers row (G10, ADR-0006).
type Ban struct {
	// IP is the banned address, as netip.Addr.String().
	IP string
	// Reason is a short human-readable reason.
	Reason string
	// BannedAt and ExpiresAt bound the ban (ExpiresAt = BannedAt + 24 h).
	BannedAt, ExpiresAt time.Time
}
