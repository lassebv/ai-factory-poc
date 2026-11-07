-- 0001_init: the §7.1 schema plus the §16.6 additions (global_stats,
-- banned_peers.expires_at). Until the first release tag this file may be
-- edited in place (§16.6 T7, ADR-0010); afterwards every change is a new file.

CREATE TABLE schema_migrations (
    version     INTEGER PRIMARY KEY,
    applied_at  TEXT NOT NULL
);

CREATE TABLE torrents (
    info_hash          TEXT PRIMARY KEY,               -- hex, lowercase
    name               TEXT NOT NULL,
    metainfo           BLOB,                           -- raw info dict; NULL until magnet metadata arrives
    magnet_uri         TEXT,                           -- original magnet, if added that way
    private            INTEGER NOT NULL DEFAULT 0,
    size_bytes         INTEGER,
    piece_length       INTEGER,
    download_dir       TEXT NOT NULL,
    desired_state      TEXT NOT NULL CHECK (desired_state IN ('active','paused','stopped')),
    stop_reason        TEXT CHECK (stop_reason IN ('user','ratio_limit','time_limit')),
    limit_override     INTEGER NOT NULL DEFAULT 0,
    bitfield           BLOB,                           -- completed pieces
    resume_fingerprint TEXT,                           -- JSON: per-file size+mtime at last flush
    uploaded_total     INTEGER NOT NULL DEFAULT 0,
    downloaded_total   INTEGER NOT NULL DEFAULT 0,
    wasted_total       INTEGER NOT NULL DEFAULT 0,
    seeding_seconds    INTEGER NOT NULL DEFAULT 0,
    ratio_limit        REAL,                           -- NULL = use global, -1 = unlimited
    seed_time_limit    INTEGER,                        -- minutes; NULL = global, -1 = unlimited
    download_limit     INTEGER NOT NULL DEFAULT 0,
    upload_limit       INTEGER NOT NULL DEFAULT 0,
    max_peers          INTEGER,
    queue_position     INTEGER NOT NULL,
    added_at           TEXT NOT NULL,
    completed_at       TEXT,
    last_error         TEXT,
    updated_at         TEXT NOT NULL
);

CREATE TABLE torrent_files (
    info_hash   TEXT NOT NULL REFERENCES torrents(info_hash) ON DELETE CASCADE,
    idx         INTEGER NOT NULL,
    path        TEXT NOT NULL,                         -- relative to download_dir
    size_bytes  INTEGER NOT NULL,
    priority    TEXT NOT NULL DEFAULT 'normal' CHECK (priority IN ('skip','low','normal','high')),
    PRIMARY KEY (info_hash, idx)
);

CREATE TABLE trackers (
    info_hash      TEXT NOT NULL REFERENCES torrents(info_hash) ON DELETE CASCADE,
    url            TEXT NOT NULL,
    tier           INTEGER NOT NULL,
    user_added     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (info_hash, url)
);

CREATE TABLE transfer_samples (                        -- one row per torrent per 10 s, kept 24 h
    info_hash      TEXT NOT NULL REFERENCES torrents(info_hash) ON DELETE CASCADE,
    ts             INTEGER NOT NULL,
    download_rate  INTEGER NOT NULL,
    upload_rate    INTEGER NOT NULL,
    PRIMARY KEY (info_hash, ts)
);

CREATE TABLE dht_nodes (                               -- routing table snapshot
    node_id   BLOB PRIMARY KEY,
    addr      TEXT NOT NULL,
    last_seen INTEGER NOT NULL
);

CREATE TABLE banned_peers (
    ip         TEXT PRIMARY KEY,
    reason     TEXT NOT NULL,
    banned_at  TEXT NOT NULL,
    expires_at TEXT NOT NULL                           -- §16.6 G10: bans expire after 24 h
);

CREATE TABLE settings (
    key    TEXT PRIMARY KEY,
    value  TEXT NOT NULL                               -- JSON-encoded
);

CREATE TABLE global_stats (                            -- §16.6 G4: single row
    id                INTEGER PRIMARY KEY CHECK (id = 1),
    uploaded_total    INTEGER NOT NULL DEFAULT 0,
    downloaded_total  INTEGER NOT NULL DEFAULT 0,
    wasted_total      INTEGER NOT NULL DEFAULT 0,
    updated_at        TEXT NOT NULL
);
