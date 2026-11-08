# Project Specification: `torrentd` — Single-Binary Torrent Client with Web UI

> **Amended 2026-11-06.** §16 resolves the findings of the spec audit (`spec/spec-audit.md`). Where §16 conflicts with §1–§15, **§16 takes precedence**. The reasons behind the human-owned decisions are in `spec/decisions/`. The executable acceptance criteria are in `spec/acceptance.yaml`.

## 1. Overview

`torrentd` is a self-contained BitTorrent client distributed as **one executable**. The binary:

1. Runs a BitTorrent engine that is **implemented from scratch in this project**. It downloads, seeds, and does peer discovery.
2. Serves a REST + real-time API.
3. Serves a React single-page application (SPA) embedded in the binary.
4. Persists all state in a local SQLite database.

The user starts the binary, opens `http://localhost:8080` in a browser, and has a fully working torrent client. Nothing else needs to be installed: no Node runtime, no separate database server, no reverse proxy.

### 1.1 Goals

- One binary, zero external runtime dependencies.
- A from-scratch implementation of the BitTorrent protocol. It must cover the functionality users expect from a mainstream client (see §4).
- A rich, responsive UI that shows live transfer statistics.
- Full lifecycle control over each torrent: add, pause, resume, **stop seeding**, recheck, and remove.
- Survives restarts: torrents, progress, and lifetime statistics are restored on startup.
- Readable, well-tested code, suitable as a reference project.

### 1.2 Non-Goals (v1)

- Torrent search, RSS feeds, and auto-download rules.
- Creating new `.torrent` files.
- BitTorrent v2 / hybrid torrents (BEP 52).
- Multi-user accounts and remote access over the internet (the app binds to localhost by default).
- Packaging as an OS service or installer.

---

## 2. Hard Constraint: No Torrent Libraries

**The BitTorrent protocol must be implemented in this codebase.** The following are **forbidden** as dependencies, directly or transitively:

- BitTorrent engines and clients (e.g. `anacrolix/torrent`, `cenkalti/rain`, libtorrent bindings).
- Bencode libraries (e.g. `jackpal/bencode-go`, `anacrolix/torrent/bencode`, `zeebo/bencode`).
- DHT / KRPC libraries (e.g. `anacrolix/dht`, `nictuku/dht`).
- Tracker client libraries, magnet-URI parsers, uTP libraries, and peer-wire libraries.

**Allowed:**

- The Go standard library, which includes `crypto/sha1`, `net`, `net/http`, `encoding/binary`, `net/url`, and `container/heap`.
- `golang.org/x/...` packages for generic needs that are not about BitTorrent (e.g. `golang.org/x/time/rate` for token buckets).
- A SQLite driver (`modernc.org/sqlite`).
- Frontend dependencies (React, etc.). The constraint only applies to the protocol.

This is checked in CI. A script fails the build if `go.mod` contains any module outside the allowlist: stdlib, `golang.org/x/*`, `modernc.org/*` and its transitive dependencies.

---

## 3. Technology Stack

| Concern              | Choice                                                                          | Rationale                                                         |
| -------------------- | ------------------------------------------------------------------------------- | ----------------------------------------------------------------- |
| Backend language     | Go (≥ 1.23)                                                                     | Single static binary, good concurrency for networking             |
| BitTorrent engine    | **Custom, in `internal/bt/...`**                                                | Hard constraint (§2)                                              |
| HTTP router          | Standard library `net/http` (Go 1.22+ pattern routing)                          | No framework needed                                               |
| Database             | SQLite via `modernc.org/sqlite`                                                 | Pure Go (no cgo), so cross-compiling stays trivial                |
| Migrations           | Embedded SQL files applied at startup (`embed.FS` + `schema_migrations` table)  | No external migration tool                                        |
| Real-time updates    | Server-Sent Events (SSE)                                                        | One-directional push, simple to serve from `net/http`             |
| Frontend             | React 19 + TypeScript, built with Vite                                          | Standard modern stack                                             |
| Frontend data layer  | TanStack Query for REST, a custom hook for SSE                                  | Caching plus live updates                                         |
| Styling              | Tailwind CSS                                                                    | Fast iteration, no CSS-in-JS runtime                              |
| Frontend embedding   | `//go:embed web/dist` served via `http.FileServerFS`                            | Ships the SPA inside the binary                                   |
| Config               | CLI flags with env var fallback                                                 | Simple                                                            |
| Logging              | `log/slog`                                                                      | Standard library                                                  |

---

## 4. Protocol Implementation (Backend Engine)

The engine lives in `internal/bt/` and is split into the packages below. Every package has unit tests. All required features count as **v1 scope**. Stretch features are planned, but v1 does not depend on them.

### 4.1 Required Feature Set

| Area                          | Spec / BEP | Requirement                                                                                      |
| ----------------------------- | ---------- | ------------------------------------------------------------------------------------------------ |
| Bencode                       | BEP 3      | Encoder and decoder supporting struct tags (`bencode:"name"`). Strict: rejects non-canonical integers and unsorted dict keys when validating the info dict. Keeps the raw bytes of `info`, so the info-hash is computed from the original bytes. |
| Metainfo (`.torrent`)         | BEP 3      | Single-file and multi-file torrents. Validates piece length, piece-hash length, and path safety (rejects `..`, absolute paths, empty components). |
| Magnet links                  | BEP 9      | Parses `xt=urn:btih:` (hex and base32), `dn`, `tr`, `x.pe`. Fetches metadata from peers using `ut_metadata` and verifies it against the info-hash. |
| HTTP/HTTPS trackers           | BEP 3, 23  | Announce with `started`, `completed`, and `stopped` events. Compact and non-compact peer lists. Honors `interval` and `min interval`. Supports tracker IDs, `failure reason`, and `warning message`. |
| UDP trackers                  | BEP 15     | Connect/announce/scrape with transaction IDs, connection-ID expiry, and retransmission with backoff (`15 * 2^n` seconds). |
| Multi-tracker                 | BEP 12     | `announce-list` tiers. Shuffles within each tier and promotes trackers that respond.            |
| Scrape                        | BEP 48     | Periodic scrape (HTTP and UDP) to show swarm seeders, leechers, and completed counts.            |
| Peer wire protocol            | BEP 3      | Handshake, all core messages, keep-alives, 16 KiB block requests, request pipelining (adaptive queue depth, 5–250 outstanding requests). |
| Fast extension                | BEP 6      | `have_all`, `have_none`, `suggest`, `reject_request`, `allowed_fast`.                             |
| Extension protocol            | BEP 10     | Extended handshake (`m`, `v`, `p`, `yourip`, `reqq`, `metadata_size`).                            |
| Peer exchange (PEX)           | BEP 11     | `ut_pex` send and receive, max one message per minute per peer, flags honored. Disabled for private torrents. |
| DHT                           | BEP 5      | Kademlia routing table (k=8 buckets, splitting), KRPC over UDP (`ping`, `find_node`, `get_peers`, `announce_peer`), token rotation every 5 min, bootstrap from well-known nodes, routing table persisted to the DB. Disabled for private torrents. |
| IPv6                          | BEP 7, 32  | IPv6 peers from trackers (`peers6`), PEX (`added6`), and DHT (`nodes6`).                          |
| Private torrents              | BEP 27     | When `private=1`, only the torrent's own trackers are used (no DHT, PEX, or LSD).                 |
| Local Service Discovery       | BEP 14     | Multicast announce on the LAN. Can be turned off in settings.                                     |
| Piece selection               | —          | Random first pieces until 4 are complete, then **rarest-first**, strict priority for file priorities (§4.3), **endgame mode** (requests the remaining blocks from all peers and cancels duplicates). |
| Choking algorithm             | BEP 3      | Tit-for-tat: 4 unchoke slots by rate, re-evaluated every 10 s. Optimistic unchoke rotated every 30 s. Seeding mode unchokes by upload rate. Anti-snubbing after 60 s without data. |
| Hash verification             | —          | SHA-1 of each piece on completion. A failed piece is discarded, and peers that send bad data repeatedly are banned (tracked per IP). |
| Storage                       | —          | Maps pieces across files, handling pieces that span file boundaries. Preallocates sparsely. Files stay as `.part` until complete (optional setting). Limits concurrently open file handles with an LRU cache. |
| Resume / fast resume          | —          | Persists the completed-piece bitfield. On startup, quickly checks file sizes and mtimes, and does a full recheck only when they don't match. |
| Force recheck                 | —          | User-triggered full re-hash of the data on disk.                                                  |
| Bandwidth limits              | —          | Global and per-torrent download and upload limits (token buckets).                                |
| Connection limits             | —          | Global max peers (default 200), per-torrent max peers (default 50), max half-open connections (default 20). |
| Queueing                      | —          | Max active downloads (default 5) and max active seeds (default 10). Extra torrents wait in `queued`. |
| Seeding limits                | —          | Per-torrent and global ratio limit and seeding-time limit. When a limit is reached, the torrent **stops seeding** (§5.2). |
| Peer ID & client identity     | —          | Azureus-style peer ID `-TD0100-` followed by 12 random bytes. Client version string in the extended handshake. |

### 4.2 Stretch Features (post-v1)

- uTP transport (BEP 29), with LEDBAT congestion control.
- Message Stream Encryption / Protocol Encryption (MSE/PE).
- Web seeds (BEP 19).
- UPnP / NAT-PMP port mapping.
- Super-seeding mode (BEP 16).

### 4.3 File Selection & Priorities

- Each file has a priority: `skip`, `low`, `normal` (default), or `high`.
- The piece picker never requests pieces that belong only to `skip` files.
- Pieces at the boundary between a skipped file and a wanted file are still downloaded. The wanted part is written. The skipped part goes to a hidden per-torrent `.parts` file so the piece can still be hash-verified.
- Priorities can be changed while the torrent is running.

### 4.4 Package Layout

```
internal/bt/
  bencode/      # encoder/decoder, raw-message type
  metainfo/     # .torrent parsing, info-hash, magnet parsing
  tracker/      # http.go, udp.go, tiered announcer, scrape
  dht/          # krpc, routing table, node, token manager, persistence hooks
  peer/         # wire messages, connection, handshake, extensions (ut_metadata, ut_pex)
  lsd/          # local service discovery
  piece/        # bitfield, piece picker (rarest-first, priorities, endgame)
  storage/      # file mapping, fd cache, verification
  choke/        # choking/unchoking algorithm
  ratelimit/    # token bucket wrappers
  session/      # orchestrates everything: per-torrent goroutines, queueing, limits
```

### 4.5 Concurrency Model

- One `Session` per process. One `Torrent` actor (goroutine plus command channel) per torrent. One read goroutine and one write goroutine per peer connection.
- Peer goroutines never touch `Torrent` state directly. They send events (`gotPiece`, `gotHave`, `disconnected`) to the torrent actor's channel.
- Disk I/O runs on a bounded worker pool (default 4 workers). Hash checking runs on a separate pool (default `GOMAXPROCS/2`).
- All goroutines take a `context.Context` and stop when it is cancelled. Tests check for leaked goroutines with a `goleak`-style helper written in-repo.

---

## 5. Domain Model

### 5.1 Torrent States

| State          | Meaning                                                                       |
| -------------- | ----------------------------------------------------------------------------- |
| `metadata`     | Added via magnet; fetching the info dict from peers                           |
| `checking`     | Verifying pieces on disk (startup mismatch, after add, or forced recheck)     |
| `queued`       | Waiting for a free download or seed slot (§4.1 Queueing)                      |
| `downloading`  | Incomplete, active, transferring or looking for peers                         |
| `seeding`      | All wanted pieces complete, active, uploading to peers                        |
| `stopped`      | **Complete, and seeding was stopped** (by the user or a seeding limit). No network activity. Data stays on disk. |
| `paused`       | Incomplete and stopped by the user. No network activity.                      |
| `error`        | Unrecoverable problem (e.g. download dir missing or unwritable, disk full)    |

### 5.2 Stop Seeding

- **Manual:** the user clicks **Stop seeding** on a torrent in `seeding` (or `queued` for seeding). The engine sends a `stopped` announce to all trackers, removes the torrent from DHT announcing, closes all peer connections, and moves it to `stopped`. `desired_state` becomes `stopped` and `stop_reason = 'user'`.
- **Automatic:** when the torrent's ratio or seeding-time limit is reached, the same thing happens with `stop_reason = 'ratio_limit'` or `'time_limit'`. Per-torrent limits override global ones. `-1` means "no limit, even if a global limit is set".
- **Resume seeding:** a `stopped` torrent can be resumed. It goes back to `seeding`, or `queued` if all seed slots are taken. If it was stopped automatically, the limit that triggered it no longer applies until the user changes it, so it doesn't stop again immediately. This is tracked with `limit_override = 1`.
- A torrent stopped from seeding is still listed, and the files stay on disk. Removing it is a separate action.

### 5.3 State Transitions

```
 add .torrent ──▶ checking ─────────────────────────────┐
 add magnet ───▶ metadata ──▶ checking                   │ (all wanted pieces done)
                                │                        ▼
                     queued ⇄ downloading ──(done)──▶ seeding ⇄ queued(seed)
                       ▲          │                       │
            resume ────┘          │ pause                 │ stop seeding / limit reached
                                  ▼                       ▼
                               paused                  stopped ──resume──▶ seeding
     any ──(I/O error)──▶ error ──(user "retry")──▶ checking
     downloading/seeding/stopped/paused ──(force recheck)──▶ checking
```

If a file priority changes from `skip` to wanted, a `seeding` or `stopped` torrent goes back to `downloading`.

### 5.4 Statistics Exposed per Torrent

| Field               | Type     | Notes                                                    |
| ------------------- | -------- | -------------------------------------------------------- |
| `id`                | string   | Info-hash, hex (40 chars), the primary key               |
| `name`              | string   | From metainfo (or magnet `dn` until metadata arrives)    |
| `state`             | enum     | §5.1                                                     |
| `stopReason`        | enum?    | `user` / `ratio_limit` / `time_limit` when `stopped`     |
| `private`           | bool     | BEP 27                                                   |
| `sizeBytes`         | int64    | Total size of **wanted** files                           |
| `completedBytes`    | int64    | Verified bytes of wanted files                           |
| `progress`          | float    | 0.0–1.0                                                  |
| `downloadRate`      | int64    | Bytes/sec, payload only, smoothed (5 s EMA)              |
| `uploadRate`        | int64    | Bytes/sec, payload only, smoothed                        |
| `uploadedTotal`     | int64    | Lifetime payload bytes uploaded, persisted               |
| `downloadedTotal`   | int64    | Lifetime payload bytes downloaded, persisted             |
| `wastedTotal`       | int64    | Bytes discarded (hash failures, duplicate endgame blocks)|
| `ratio`             | float    | `uploadedTotal / max(downloadedTotal, completedBytes)`   |
| `peersConnected`    | int      | Currently connected peers                                |
| `seedsConnected`    | int      | Connected peers that have every piece                    |
| `swarmSeeders`      | int?     | From the latest scrape                                   |
| `swarmLeechers`     | int?     | From the latest scrape                                   |
| `etaSeconds`        | int64?   | `null` unless downloading with rate > 0                  |
| `downloadDir`       | string   | Absolute path                                            |
| `addedAt`           | RFC3339  |                                                          |
| `completedAt`       | RFC3339? | When the torrent first reached 100%                      |
| `seedingSeconds`    | int64    | Total time spent in `seeding`, persisted                 |
| `ratioLimit`        | float?   | Effective limit (per-torrent or global)                  |
| `seedTimeLimitMin`  | int?     | Effective limit in minutes                               |
| `downloadLimit`     | int64    | Per-torrent bytes/sec, 0 = unlimited                     |
| `uploadLimit`       | int64    | Per-torrent bytes/sec, 0 = unlimited                     |
| `errorMessage`      | string?  | Set when `state = error`                                 |

---

## 6. Engine Services Around the Protocol

**Stats Sampler** (`internal/stats`)
- Runs on a 1-second ticker and reads counters from `session`.
- Computes smoothed rates and publishes snapshots to an in-memory broadcaster (for SSE).
- Every 10 seconds (and on shutdown), writes cumulative totals, seeding time, and the piece bitfield to SQLite. It also writes a speed sample.
- Checks seeding limits on each tick and tells the session to stop seeding when a limit is reached.

**Store** (`internal/store`)
- A repository layer over `database/sql`. All SQL lives here.
- WAL mode, `busy_timeout=5000`, `foreign_keys=ON`.

**HTTP Layer** (`internal/api`)
- REST handlers, SSE, and static file serving. Unknown non-`/api` paths fall back to `index.html`.

### 6.1 Startup Sequence

1. Parse config. Create the data directory if it is missing.
2. Open SQLite and apply migrations.
3. Start the session: bind the peer TCP listener and the UDP socket (DHT and UDP trackers share one socket, demultiplexed by first byte/transaction). Load the DHT routing table from the DB, then bootstrap.
4. Load torrents from the DB. Restore the bitfield and fast-resume state. Apply `desired_state` (`active` / `paused` / `stopped`) through the queueing rules.
5. Start the stats sampler, then the HTTP server. Optionally open the browser (`--open`).

### 6.2 Shutdown (SIGINT/SIGTERM)

> **Amended by §16.11:** steps 1 and 2 start at the same time.

1. Graceful HTTP shutdown (5 s).
2. Send `stopped` announces to trackers, in parallel, with a 3 s timeout.
3. Flush stats, bitfields, and the DHT routing table to the DB.
4. Close peers, flush and close files, then close the DB.

---

## 7. Persistence (SQLite)

Database file: `<dataDir>/torrentd.db`. Default `dataDir`: `$XDG_DATA_HOME/torrentd` on Linux, `~/Library/Application Support/torrentd` on macOS, `%APPDATA%\torrentd` on Windows.

### 7.1 Schema (migration `0001_init.sql`)

```sql
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
    banned_at  TEXT NOT NULL
);

CREATE TABLE settings (
    key    TEXT PRIMARY KEY,
    value  TEXT NOT NULL                               -- JSON-encoded
);
```

### 7.2 Rules

- `desired_state` records what the user asked for. The live `state` is derived at runtime and never stored.
- Lifetime counters on each flush are `value at session start + session delta`, so restarts never reset them.
- Samples older than 24 h are deleted hourly.
- **Default settings:** `default_download_dir` (`~/Downloads`), `listen_port` (42069), `global_download_limit` / `global_upload_limit` (0), `max_peers_global` (200), `max_peers_per_torrent` (50), `max_active_downloads` (5), `max_active_seeds` (10), `global_ratio_limit` (null), `global_seed_time_limit` (null), `dht_enabled` (true), `pex_enabled` (true), `lsd_enabled` (true), `use_part_files` (true), `recent_download_dirs` ([]).

---

## 8. HTTP API

Base path: `/api/v1`. JSON throughout. Errors use this shape:

```json
{ "error": { "code": "invalid_torrent", "message": "file is not a valid .torrent" } }
```

### 8.1 Endpoints

| Method   | Path                                     | Description                                                       |
| -------- | ---------------------------------------- | ----------------------------------------------------------------- |
| `GET`    | `/api/v1/torrents`                       | List all torrents (§5.4 objects)                                  |
| `POST`   | `/api/v1/torrents`                       | Add from a `.torrent` file or magnet URI (§8.2)                   |
| `POST`   | `/api/v1/torrents/preview`               | Parse a `.torrent` without adding it: `{name, sizeBytes, files[], trackers[], private}` |
| `GET`    | `/api/v1/torrents/{id}`                  | Detail: §5.4 plus `files[]`, `trackers[]`, `peers[]`, `pieces` (base64 bitfield) |
| `PATCH`  | `/api/v1/torrents/{id}`                  | Update per-torrent settings: limits, ratio/time limits, max peers, `downloadDir` (moves data) |
| `POST`   | `/api/v1/torrents/{id}/pause`            | Pause (incomplete torrent). `204`                                 |
| `POST`   | `/api/v1/torrents/{id}/resume`           | Resume a paused torrent, or **resume seeding** a stopped one. `204` |
| `POST`   | `/api/v1/torrents/{id}/stop-seeding`     | **Stop seeding.** `409 not_complete` if the torrent isn't complete. `204` |
| `POST`   | `/api/v1/torrents/{id}/recheck`          | Force a full hash check. `204`                                    |
| `POST`   | `/api/v1/torrents/{id}/reannounce`       | Announce to all trackers now (respects `min interval`). `204`     |
| `POST`   | `/api/v1/torrents/{id}/queue`            | `{ "position": "top" \| "up" \| "down" \| "bottom" }`              |
| `DELETE` | `/api/v1/torrents/{id}`                  | Remove. `?deleteData=true` also deletes the files. `204`          |
| `PATCH`  | `/api/v1/torrents/{id}/files`            | `[{ "idx": 3, "priority": "skip" }]`                              |
| `POST`   | `/api/v1/torrents/{id}/trackers`         | Add tracker `{url, tier}`                                         |
| `DELETE` | `/api/v1/torrents/{id}/trackers?url=...` | Remove a tracker                                                  |
| `GET`    | `/api/v1/torrents/{id}/history`          | Speed samples, last 24 h                                          |
| `POST`   | `/api/v1/torrents/bulk`                  | `{ ids: [...], action: "pause"\|"resume"\|"stop-seeding"\|"recheck"\|"remove", deleteData? }` |
| `GET`    | `/api/v1/stats`                          | Global: rates, counts per state, session and lifetime totals, DHT node count, listen port status |
| `GET`    | `/api/v1/settings`                       | Current settings                                                  |
| `PATCH`  | `/api/v1/settings`                       | Partial update. Applied live (limits, peers, DHT/PEX/LSD toggles) |
| `GET`    | `/api/v1/fs/validate?path=...`           | `{exists, writable, freeBytes}`                                   |
| `GET`    | `/api/v1/events`                         | SSE stream (§8.3)                                                 |
| `GET`    | `/healthz`                               | `200 ok`                                                          |

### 8.2 Add Torrent

`POST /api/v1/torrents` takes `multipart/form-data` with **either** `file` (a `.torrent`, max 10 MB) **or** `magnet` (a URI). Optional fields: `downloadDir`, `startPaused`, and `filePriorities` (JSON array by index, only with `file`).

Responses:

| Status | Code                    | When                                                               |
| ------ | ----------------------- | ------------------------------------------------------------------ |
| `201`  | —                       | Added. Returns the torrent object.                                 |
| `400`  | `invalid_torrent`       | Not valid bencoded metainfo, or failed validation (§4.1)           |
| `400`  | `invalid_magnet`        | Not a valid magnet URI or missing `btih`                           |
| `400`  | `invalid_download_dir`  | Not absolute, or cannot be created or written to                   |
| `409`  | `already_exists`        | Same info-hash already present. The body includes `id`. New trackers from the input are merged into the existing torrent. |

### 8.3 Server-Sent Events

| Event              | Frequency      | Payload                                                         |
| ------------------ | -------------- | --------------------------------------------------------------- |
| `snapshot`         | every 1 s      | `{ global, torrents: TorrentStats[] }`                          |
| `torrent.added`    | on change      | Full torrent object                                             |
| `torrent.removed`  | on change      | `{ id }`                                                        |
| `torrent.state`    | on transition  | `{ id, from, to, stopReason?, errorMessage? }`                  |
| `torrent.metadata` | magnet resolved| Full torrent object (now with files)                            |

A `: ping` comment is sent every 15 s. The client relies on `EventSource` auto-reconnect, and the next `snapshot` restores full state.

### 8.4 Peer & Tracker Objects

```json
{ "addr": "1.2.3.4:51413", "client": "qBittorrent 4.6.0", "progress": 0.42,
  "downloadRate": 120000, "uploadRate": 30000, "source": "dht",
  "flags": { "choked": false, "interested": true, "peerChoked": true, "peerInterested": true,
             "snubbed": false, "optimistic": false, "incoming": true } }

{ "url": "udp://tracker.example:6969/announce", "tier": 0, "status": "working",
  "lastAnnounce": "2026-11-06T22:10:00Z", "nextAnnounce": "2026-11-06T22:40:00Z",
  "seeders": 812, "leechers": 95, "downloaded": 20311, "message": null }
```

---

## 9. Frontend

### 9.1 Structure

```
web/src/
  api/          # typed fetch client, useLiveStats (SSE) hook
  components/   # TorrentTable, AddTorrentDialog, StateBadge, SpeedChart, PieceBar, FileTree, ...
  pages/        # TorrentsPage, TorrentDetailPage, SettingsPage
  lib/format.ts # bytes, rates, durations, ETA
```

Routing: `/` (list), `/torrents/:id` (detail), `/settings`. The Vite dev server proxies `/api` to `http://localhost:8080`.

### 9.2 Global Layout

- **Header:** app name, global ↓/↑ speed with limit indicator, and buttons for "Add torrent" and Settings.
- **Footer status bar:** SSE connection indicator, DHT node count, incoming-port reachability (based on whether incoming connections were seen), and free disk space in the default dir.

### 9.3 Feature: Add a Torrent

Opened from the header button, by dragging a `.torrent` file onto the window, or by pasting a magnet link anywhere (global paste listener).

1. **Source:** a tab with "File" (drop zone or picker) and a tab with "Magnet link" (text input, validated on the client side).
2. **Preview:** for a file, shows the name, size, private flag, and a **file tree with checkboxes and priority** selectors. For a magnet, shows "Metadata will be fetched from peers". After adding, file selection is available on the detail page.
3. **Download location:** a text input pre-filled with the default dir and with recent dirs as suggestions.
   - Validated live via `/fs/validate` (debounced 300 ms), showing writable status and free space.
   - Warns when free space is smaller than the selected size.
   - A "Remember as default" checkbox updates the setting.
4. **Options:** "Start paused".
5. **Add:** shows a toast and briefly highlights the new row. A `409` shows "Already added", with a link to the existing torrent.

> Browsers can't open a native folder picker that returns a server-side path, so the location is a text input with suggestions.

### 9.4 Feature: Torrent List

A table that updates live from SSE `snapshot`s.

| Column    | Downloading                         | Seeding                       | Stopped (seeding stopped)        |
| --------- | ----------------------------------- | ----------------------------- | -------------------------------- |
| Name      | name (lock icon if private)         | name                          | name                             |
| State     | blue badge                          | green badge                   | grey badge + reason tooltip ("Ratio limit 2.0 reached") |
| Progress  | bar with %                          | full bar                      | full bar                         |
| Size      | `1.2 GB of 4.0 GB`                  | `4.0 GB`                      | `4.0 GB`                         |
| ↓ Speed   | `3.4 MB/s`                          | —                             | —                                |
| ↑ Speed   | `120 KB/s`                          | `850 KB/s`                    | —                                |
| Peers     | `12 (4 seeds)`, with swarm `812/95` on hover | `5`                  | —                                |
| ETA       | `14m 32s`                           | —                             | —                                |
| Uploaded  | total                               | total                         | total                            |
| Ratio     | `0.12`                              | `1.84`                        | `2.00`                           |
| Seed time | —                                   | `3d 4h`                       | `3d 4h`                          |
| Added     | relative time, absolute on hover    | same                          | same                             |

- **Filter tabs with counts:** All · Downloading · Seeding · Stopped · Paused · Queued · Error.
- Sorting on any column, name search, and **multi-select with bulk actions**.
- **Row actions by state:**
  - Downloading/queued: **Pause**.
  - Paused: **Resume**.
  - Seeding: **Stop seeding**.
  - Stopped: **Resume seeding**.
  - All states: Recheck, Reannounce, Move up/down in queue, Remove (with an "Also delete downloaded files" checkbox, unchecked by default).
- Right-click context menu with the same actions.
- Empty state with a call to action. Card layout below 768 px.

### 9.5 Feature: Torrent Detail

- **Overview:** transfer stats, general info (info-hash with copy button, magnet link with copy button, piece count and size, private flag, dates), a **piece map bar** (rendered on canvas from the bitfield), and a **speed chart** (24 h history plus live points).
- **Files:** a tree with size, progress, and a priority dropdown (`skip`/`low`/`normal`/`high`). Bulk select is supported.
- **Peers:** a live table with address, client, flags, progress, ↓/↑, and source (tracker/DHT/PEX/LSD/incoming). Polled every 2 s while open.
- **Trackers:** status, last/next announce, scrape counts, and messages. Add/remove trackers and Reannounce.
- **Options:** per-torrent download/upload limit, max peers, ratio limit, seeding time limit (each can be set to "use global" or "unlimited"), and move download location.

### 9.6 Settings Page

Sections:

- **Downloads:** default dir, part files.
- **Connection:** listen port, max peers global/per-torrent, half-open limit.
- **Bandwidth:** global ↓/↑ limits.
- **Queueing:** max active downloads/seeds.
- **Seeding:** global ratio limit, global seeding time limit.
- **Privacy & discovery:** DHT, PEX, LSD toggles.

Changes apply without restarting, except the listen port, which rebinds live and shows an error if the port is busy.

### 9.7 Formatting Rules

- Sizes are computed in 1024s and labelled KB/MB/GB, with one decimal.
- Speeds below 1 KB/s show as `0 KB/s`. Inactive torrents show `—`.
- ETA: `∞` when the rate is 0 while downloading. Use at most two units (`2h 14m`).

---

## 10. Configuration

| Flag            | Env var               | Default             | Description                                  |
| --------------- | --------------------- | ------------------- | -------------------------------------------- |
| `--addr`        | `TORRENTD_ADDR`       | `127.0.0.1:8080`    | HTTP listen address                          |
| `--data-dir`    | `TORRENTD_DATA_DIR`   | OS default (§7)     | Location of the DB                           |
| `--peer-port`   | `TORRENTD_PEER_PORT`  | from settings       | TCP+UDP port for peers/DHT                   |
| `--open`        | —                     | `false`             | Open the browser on startup                  |
| `--log-level`   | `TORRENTD_LOG_LEVEL`  | `info`              | `debug` / `info` / `warn` / `error`          |
| `--dev`         | —                     | `false`             | Don't serve the embedded SPA; enable CORS for the Vite dev server |

---

## 11. Non-Functional Requirements

- **Throughput:** saturates a 100 Mbit/s link on a well-seeded torrent. Sustains at least 50 MB/s over loopback between two `torrentd` instances.
- **Scale:** handles 200 torrents and 500 total peer connections with a responsive UI. The SSE snapshot for 200 torrents stays under 100 KB/s. Idle CPU is under 2%. RSS is under 200 MB with 200 torrents.
- **Robustness:** tolerates malformed input from peers, trackers, and DHT. Bencode and wire parsers enforce size limits and never panic (fuzz-tested). Messages over 16 KiB + 13 bytes (except bitfield/metadata) are rejected and the peer is disconnected.
- **Reliability:** `kill -9` loses at most 10 s of counters and bitfield progress (pieces are re-verified via fast resume) and never corrupts the DB.
- **Interoperability:** verified against at least qBittorrent (libtorrent) and Transmission, as both seeder and leecher, and against public HTTP and UDP trackers.
- **Security:**
  - Binds to `127.0.0.1` by default, and logs a warning on non-loopback binds.
  - Metainfo paths are sanitized so a torrent can never write outside its `download_dir`.
  - `deleteData` only removes files belonging to the torrent.
  - `Origin` checks on state-changing requests (CSRF defense).
  - Upload limit of 10 MB.
- **Portability:** linux/amd64, linux/arm64, darwin/arm64, windows/amd64 with `CGO_ENABLED=0`.
- **Binary size:** under 40 MB.

---

## 12. Repository Layout

```
.
├── cmd/torrentd/main.go          # wiring, flags, startup/shutdown
├── internal/
│   ├── bt/                       # protocol implementation (§4.4)
│   ├── api/                      # handlers, SSE broadcaster, middleware
│   ├── stats/                    # sampler, rate smoothing, seeding-limit enforcement
│   ├── store/                    # SQLite repository + migrations/*.sql
│   └── config/
├── tools/
│   ├── depcheck/                 # enforces §2 allowlist
│   └── swarm/                    # local test tracker + seeder for E2E
├── web/                          # React app (Vite); dist/ is embedded
├── webembed.go                   # //go:embed web/dist
├── Makefile
└── project-description.md
```

Build: `make build` runs `npm ci && npm run build` in `web/`, then `CGO_ENABLED=0 go build ./cmd/torrentd`. A placeholder `web/dist/index.html` is committed, so `go build` works without building the frontend first.

---

## 13. Testing Strategy

| Layer          | Approach                                                                                           |
| -------------- | -------------------------------------------------------------------------------------------------- |
| Bencode        | Table tests and round-trip tests. `go test -fuzz` on the decoder (no panics, decode→encode is stable). |
| Metainfo       | Real `.torrent` fixtures (single-file, multi-file, private, announce-list). Path-traversal rejection. Info-hash matches known values. |
| Magnet         | Hex/base32 parsing. `ut_metadata` fetch against a fake peer.                                       |
| Trackers       | `httptest` HTTP tracker. In-process UDP tracker fake with drops and retransmits.                   |
| Peer wire      | Message encode/decode fuzzing. Handshake over `net.Pipe`. Fast extension and BEP 10 tests.         |
| Piece picker   | Deterministic tests for rarest-first, priorities, skip files, endgame duplicate cancellation.      |
| Choker         | Simulated peers with scripted rates. Asserts unchoke sets and optimistic rotation.                 |
| DHT            | Routing-table bucket splitting. A 50-node in-process DHT network on loopback finds peers for an info-hash. |
| Storage        | Pieces spanning file boundaries, part files, skip-file boundary pieces, recheck.                   |
| Swarm (integration) | `tools/swarm` starts a tracker, 1 seeder, and 3 leechers, all in-process. Verifies completion, hash correctness, and that **stop seeding** stops all uploads and sends a `stopped` announce. Verifies that a ratio limit triggers an automatic stop. |
| Interop (manual/CI-optional) | Docker Compose with qBittorrent-nox and Transmission, exchanging a generated torrent both ways. |
| API            | `httptest` for every endpoint and error code.                                                      |
| Frontend       | Vitest + React Testing Library: formatting, table rendering per state, action buttons per state, add-dialog flows, with a mock SSE source. |
| End-to-end     | Playwright: start the binary and the local swarm. Add a torrent, wait for Seeding, click **Stop seeding** and assert Stopped, then resume. Restart the binary and verify state and totals persisted. |

---

## 14. Acceptance Criteria

1. `./torrentd` starts with no arguments, and `http://127.0.0.1:8080` shows the UI.
2. `go.mod` contains no BitTorrent, bencode, DHT, or tracker libraries, and `tools/depcheck` passes in CI.
3. A user can add a `.torrent` file **or a magnet link**, choose a download folder, optionally deselect files, and see the torrent in the list within 1 second.
4. While downloading, the list shows ↓/↑ speed, progress, ETA, and connected peers/seeds, updating every second.
5. After completion, the torrent moves to **Seeding** automatically and shows upload speed, total uploaded, ratio, seeding time, and added date.
6. **Stop seeding** moves a seeding torrent to **Stopped**: no further upload, a `stopped` event sent to trackers, and files kept. **Resume seeding** brings it back.
7. When a configured ratio or seeding-time limit is reached, seeding stops automatically, and the UI shows the reason.
8. Pause/resume, recheck, reannounce, queue reordering, file priorities, per-torrent and global speed limits, and remove (with and without data) all work and show up in the UI immediately.
9. Peers are discovered via HTTP trackers, UDP trackers, DHT, PEX, and LSD. Private torrents use trackers only.
10. A torrent downloaded by `torrentd` can be seeded to qBittorrent and Transmission, and the other way around.
11. After a restart, all torrents reappear with their state (active/paused/stopped), progress (without a full recheck when files are unchanged), and lifetime totals.
12. Invalid files, bad magnets, duplicates, and unwritable directories produce clear inline errors.
13. The binary builds with `CGO_ENABLED=0` and runs with no files besides itself.

---

## 15. Suggested Milestones

1. **Skeleton:** Go server with the embedded placeholder SPA, SQLite and migrations, `/healthz`, depcheck in CI.
2. **Bencode + metainfo:** parser, encoder, and fuzz tests. `/torrents/preview` works.
3. **Single-peer download:** HTTP tracker, peer wire, storage, and hash verification. Downloads a torrent from one seeder.
4. **Swarm engine:** multiple peers, piece picker (rarest-first and endgame), choking, seeding, persistence and fast resume, stop seeding.
5. **Discovery:** UDP trackers, multi-tracker, scrape, DHT, PEX, LSD, magnet links with `ut_metadata`.
6. **Live stats + API:** sampler, SSE, all REST endpoints, limits and queueing, seeding limits.
7. **UI:** list with per-state actions and bulk actions, add dialog (file and magnet), detail page tabs, settings.
8. **Hardening:** fast extension, IPv6, peer banning, interop tests, E2E, cross-platform builds, performance targets.
9. **Stretch:** uTP, MSE/PE, web seeds, UPnP/NAT-PMP.

---

## 16. Spec Amendments (spec audit, 2026-11-06)

These amendments are normative. Audit IDs are given in brackets, and the reasoning for each is in `spec/spec-audit.md`. ADR numbers point to `spec/decisions/`.

### 16.1 Scope and Metainfo (amends §1.2, §4.1)

- **[C2, ADR-0001] Hybrid torrents.** Hybrid (v1+v2) torrents are accepted and handled through their v1 view only: v1 info-hash and `pieces`. The fields `meta version`, `file tree` and `piece layers` are ignored. BEP 47 padding files (`attr` contains `p`) are never written to disk, are excluded from all sizes and from every file list shown to the user, and are treated as zeros when hashing. v2-only torrents are rejected with `invalid_torrent`. Full BEP 52 remains a non-goal.
- **[A2] Piece length.** Must be between 16 KiB and 256 MiB. A power of two is not required, but a non-power-of-two length is logged. `len(pieces)` must equal `20 × ceil(total / pieceLength)`.
- **[A3] Paths.**
  - Reject `..`, absolute paths and empty components.
  - Reject duplicate paths after case-folding and Unicode NFC normalization.
  - Sanitize names that are invalid on Windows (reserved names such as `CON` and `NUL`, the characters `<>:"|?*`, trailing dots and spaces) by replacing them with `_` **on all platforms**.
  - Prefer `path.utf-8` and `name.utf-8` when present.
- **[A1] Bencode modes.**
  - **Strict** (canonical integers, sorted unique keys): used for the info dict and the `ut_metadata` payload.
  - **Lenient** (accepts unsorted keys, still rejects malformed input): used for tracker, DHT and extension messages.
  - Both modes enforce the size and depth caps in §16.2.

### 16.2 Protocol Engine (amends §4.1, §4.3, §6.1)

- **[C5] UDP sockets.** One UDP socket per address family (`udp4`, `udp6`), each shared by DHT and UDP trackers. Packets are demultiplexed by first byte: `d` means KRPC, anything else is a tracker reply matched by transaction ID. Each family has its own DHT routing table (BEP 32).
- **[G3] DHT node IDs.** Persisted per family in the internal settings keys `dht_node_id_v4` and `dht_node_id_v6`. If an ID is missing or regenerated, the stored routing table for that family is discarded.
- **[A4] Multi-tracker.** Strict BEP 12: try tiers in order, shuffle within each tier, promote trackers that respond. User-added trackers go in the tier given by the user.
- **[A10] Scrape.** Every 30 min, plus once after the first successful announce. The HTTP scrape URL is derived using the BEP 48 convention. If it can't be derived, there is no HTTP scrape.
- **[A5] Piece priority and endgame.**
  - A piece's priority is the maximum priority of the wanted files it overlaps.
  - Pick strictly by class, `high` > `normal` > `low`, using rarest-first within each class.
  - Random-first-4 applies within the highest available class.
  - Endgame starts when every remaining wanted block has been requested at least once.
- **[A7] Bandwidth limits.** Limits apply to all peer-wire bytes (payload plus protocol overhead). Displayed rates remain payload-only. Tracker, DHT and LSD traffic are not limited.
- **[G10, ADR-0006] Peer banning.**
  - Record which peers contributed blocks to each piece. On a hash failure, every contributor gets a strike.
  - An IP is banned at 3 strikes. Bans expire after 24 h.
  - No ban UI in v1. `/stats` reports `bannedPeers`.
- **[G15, ADR-0007] Private magnets.** DHT, PEX and LSD may be used while a torrent is in `metadata`. If the metadata shows `private=1`, disconnect all peers whose source is `dht`, `pex` or `lsd`, and permanently stop DHT, PEX and LSD for that torrent.
- **[G22] Completion.** `completedAt` is set the first time the torrent reaches 100%, including when the initial check finds it already complete. The `completed` tracker event is sent only when completion happened through downloading in the current session.
- **[G21] Part files.**
  - Each file is named `<file>.part` until all of *its* pieces are verified, then renamed.
  - Data for boundary pieces (§4.3) goes in `<download_dir>/.torrentd-<infohash>.parts`.
  - When a file changes from `skip` to wanted, its bytes are copied out of `.parts` into the file.
  - Toggling `use_part_files` affects only incomplete files.
  - `deleteData` also removes `.part` and `.parts` files and any empty directories the torrent created.
- **[T6] Size caps.**
  - `metadata_size` ≤ 10 MiB.
  - Piece count ≤ 2²¹, so a bitfield is at most 256 KiB.
  - A bitfield received before the metadata is buffered and validated once the metadata arrives.
  - Bencode nesting depth ≤ 64.
  - An incoming `request` for more than 16 KiB gets a `reject_request`, or the peer is disconnected if it doesn't support the fast extension.
- **[D1–D14] Engineering defaults.** The table in `spec/spec-audit.md` §D is normative (pipeline depth, BEP 6 behavior, PEX limits, DHT bootstrap nodes and announce cadence, LSD groups, announce parameters, UDP retransmission, worker pools, enums, client identity, swarm counts).

### 16.3 Resume and Crash Recovery (amends §4.1, §6.1, §11)

- **[C3, ADR-0002] Crash recovery.**
  - A clean-shutdown marker (internal setting `clean_shutdown`) is written on graceful shutdown and cleared at startup.
  - Piece data **must be fsynced before** a bitfield containing those pieces is persisted.
  - On startup after a clean shutdown, check file sizes and mtimes (§4.1). After an unclean shutdown, trust the persisted bitfield and check sizes only.
  - Pieces completed after the last flush are downloaded again.
- **[G8, ADR-0004] Missing files.** If the bitfield is non-empty and **all** of a torrent's files are missing, the torrent goes to `error` with the message "Files missing". If only some files are missing, recheck and download the missing data again.

### 16.4 States and Lifecycle (amends §4.1 Queueing, §5)

- **[G9, ADR-0005] New state `moving`.** Data is being moved after `PATCH downloadDir`. There is no network activity.
  - On the same filesystem, rename. Otherwise copy, fsync, then delete the source, in the background. Then return to the desired state.
  - If any destination file already exists, return `409 target_exists`.
  - Pause, recheck, move and remove during `moving` return `409 invalid_state`.
- **[C6] Rechecking a stopped torrent.** If a recheck of a `stopped` torrent finds missing pieces, the torrent goes to `paused` (`desired_state=paused`, with `stop_reason` and `limit_override` cleared).
- **[G1] Retry.** `POST /resume` on an `error` torrent means retry, which moves it to `checking`. The UI labels the button "Retry".
- **[C4, ADR-0003] `limit_override`.** When set, both the ratio limit and the time limit are suspended for that torrent. It is cleared only by a PATCH to that torrent's `ratioLimit` or `seedTimeLimitMin`.
- **[A6] Seeding limits.** The ratio uses the §5.4 formula and is `0` when the denominator is 0. The time limit uses the cumulative `seedingSeconds`. Limits are evaluated only in `seeding`.
- **[G11] Queueing.**
  - Download slots are used by `downloading` and `metadata` torrents. Stalled torrents still count.
  - One global queue order applies separately within the download class and the seed class.
  - At most one torrent is hash-checked at a time. The others wait in `checking` with `checkingProgress = 0`.
  - A completed torrent moves to `seeding` through the seed queue, so it may go to `queued`.
- **[G18] Action validity.**

  | Action | Rule |
  | --- | --- |
  | `pause` | Allowed in `metadata`, `checking`, `queued`, `downloading`. On a complete torrent: `409 invalid_state`. |
  | `stop-seeding` | On an incomplete torrent: `409 not_complete`. |
  | `resume` | On a torrent that is already active: `204`, no-op. On `error`: retry. |
  | `recheck`, `PATCH /files` | In `metadata`: `409 no_metadata`. |
  | Any action except `GET` | In `moving`: `409 invalid_state`. |

### 16.5 Statistics (amends §5.4, §6)

| Field | Type | Notes |
| --- | --- | --- |
| `state` | enum | Gains `moving` [G9] |
| `hasMetadata` | bool | [G19] |
| `sizeBytes`, `completedBytes` | int64? | `null` until metadata arrives [G19] |
| `checkingProgress` | float? | 0–1, `null` unless `checking` [G6] |
| `seedsConnected` | int | `0` until metadata arrives [G19] |
| `swarmSeeders`, `swarmLeechers` | int? | Maximum across trackers that responded recently [D14] |
| *(detail only)* `ratioLimitSetting`, `seedTimeLimitSetting` | number? | Raw per-torrent value: `null` = global, `-1` = unlimited [G7] |
| *(detail only)* `maxPeers`, `limitOverride`, `queuePosition`, `pieceLength`, `pieceCount`, `magnetUri` | — | `magnetUri` is generated for torrents added from a file [G7] |

- **[A12] Speed samples.** Each sample is the *average* rate over its 10 s window. Rows where both rates are 0 are skipped, and consumers treat gaps as 0.
- **[G4] Global totals.** Global lifetime totals are persisted in `global_stats` (§16.6) and are not derived from the current torrents.

### 16.6 Persistence (amends §7)

```sql
CREATE TABLE global_stats (                            -- [G4] single row
    id                INTEGER PRIMARY KEY CHECK (id = 1),
    uploaded_total    INTEGER NOT NULL DEFAULT 0,
    downloaded_total  INTEGER NOT NULL DEFAULT 0,
    wasted_total      INTEGER NOT NULL DEFAULT 0,
    updated_at        TEXT NOT NULL
);

-- banned_peers gains [G10]:
--     expires_at  TEXT NOT NULL
```

- **[G2]** Add the default setting `max_half_open` (20).
- **[G3, C3]** Internal settings keys `dht_node_id_v4`, `dht_node_id_v6` and `clean_shutdown` are never returned by or accepted from `/settings`.
- **[T7, ADR-0010] Schema changes.** Until the first release tag, `0001_init.sql` may be edited directly. After that, every change is a new migration. All schema changes go through the architect role.

### 16.7 HTTP API (amends §8)

- **[G20] OpenAPI contract.** `spec/openapi.yaml` is the authoritative contract for every payload. The architect role writes it before any backend or frontend work starts, and the human reviews it.
- **[C1] Slim snapshot.** `snapshot.torrents[]` contains only fields that change: `id`, `state`, `stopReason`, `progress`, `completedBytes`, `checkingProgress`, `downloadRate`, `uploadRate`, `uploadedTotal`, `downloadedTotal`, `wastedTotal`, `ratio`, `peersConnected`, `seedsConnected`, `etaSeconds`, `seedingSeconds`. Static fields come from REST and the `torrent.added`/`torrent.metadata` events.
- **[G12] Error codes.** `not_found`, `invalid_request`, `invalid_state`, `not_complete`, `no_metadata`, `already_exists`, `invalid_torrent`, `invalid_magnet`, `invalid_download_dir`, `target_exists`, `port_in_use`, `payload_too_large`, `forbidden_origin`, `forbidden_host`, `internal`.
- **[G13] Bulk.** Returns `200 {"results":[{"id","ok":true} | {"id","ok":false,"error":{…}}]}`. A failure on one ID does not stop the others.
- **[G14] Torrent file for a pending magnet.** Adding a `.torrent` for a magnet still in `metadata`: verify the file's info dict against the info-hash, use it as the metadata, merge the trackers, and return `409 already_exists` with the `id`.
- **[G9] Moving data.** `PATCH /torrents/{id}` with `downloadDir` returns `202` when a move starts, or `200` for a torrent in `metadata`.
- **[G16] Peer port.**
  - `--peer-port` overrides `listen_port` for the current process, is not persisted, and makes the setting read-only in the UI.
  - If the port is busy at startup, the HTTP UI still starts and `/stats` reports the error.
  - If the port is busy on PATCH, the old binding is kept, the setting is not saved, and the response is `409 port_in_use`.
- **[G17] Host and Origin checks.**
  - Every request needs a `Host` header from the allowlist (`localhost`, `127.0.0.1`, `[::1]`, the host of `--addr`, each with the configured port), otherwise `403 forbidden_host`.
  - On state-changing requests, an `Origin` header that is present must be on the allowlist, otherwise `403 forbidden_origin`. A missing `Origin` is allowed.
  - `--dev` adds the Vite dev origin to the allowlist.
- **[A8] Reannounce.** Before `min interval` has passed, return `204` and schedule the announce for the earliest allowed time.
- **[A9] PATCH semantics.** JSON Merge Patch (RFC 7386). A missing field is unchanged. For limits, `null` means "use global" and `-1` means "unlimited". Unknown fields return `400 invalid_request`.
- **[A11] `/fs/validate`.**
  - `writable` means the path can be written, or, if it doesn't exist, that it can be created in its nearest existing ancestor.
  - `freeBytes` is measured on that same directory.
  - A non-absolute path returns `400 invalid_download_dir`.

### 16.8 Frontend (amends §9)

- **[G5] States not covered by §9.4:**

  | State | Progress | ↓ / ↑ | Peers | ETA | State-specific action |
  | --- | --- | --- | --- | --- | --- |
  | `paused` | bar with % | — | — | — | Resume |
  | `queued`, incomplete | bar with % | — | — | — | Pause |
  | `queued`, complete | full bar | — | — | — | Stop seeding |
  | `checking` | bar showing `checkingProgress` ("Checking 42%", or "Waiting to check") | — | — | — | Pause |
  | `metadata` | indeterminate, "Fetching metadata" | speeds | peers | — | Pause |
  | `moving` | indeterminate, "Moving…" | — | — | — | *(none; all actions disabled)* |
  | `error` | bar with % | — | — | — | Retry (`errorMessage` inline or on hover) |

- **[A15] Formatting.**
  - Sizes under 1 KB show as `512 B`. `TB` is used above `GB`.
  - Durations use at most two units, up to days (`3d 4h`). An ETA over 365 days shows as `∞`.
  - "Inactive" means any state other than `downloading`, `seeding` or `metadata`.
- **[A16] Global paste listener.** Ignores paste events in inputs, textareas and contenteditable elements. It opens the dialog only for text matching `^magnet:\?`.

### 16.9 Non-Functional Requirements (amends §11)

- **[A13, ADR-0008]** Each NFR is a concrete benchmark scenario on a named CI runner class. The scenarios are in ADR-0008.
- **[C7]** The 500-connection scale scenario sets `max_peers_global = 500`.

### 16.10 Quality Gates and Testability (amends §13)

- **[A14, ADR-0009] Quality gates.**
  - **Blocking:** coverage ≥ 80% per `internal/bt/*` package and ≥ 60% elsewhere; `go vet` and staticcheck clean; doc comments on every exported identifier; no Go file over 800 lines.
  - **Advisory only:** the reviewer agent's score.
- **[T1] Injected clock.** Time comes from an `internal/clock` interface. Direct use of `time.Now`, `time.After`, `time.Tick`, `time.NewTimer` and `time.NewTicker` is forbidden in `internal/bt/**`, `internal/stats` and `internal/store`. An L0 lint check enforces this.
- **[T2] Injected randomness.** Randomness comes from an injected source (peer ID, piece selection, tier shuffle, DHT and transaction IDs), so tests are deterministic.
- **[T3] Hermetic tests.** Tiers L0–L4 use loopback only. DHT bootstrap nodes, tracker URLs and the LSD multicast group and interface are configurable, and LSD tests use an injectable transport.
- **[T4] Interop containers.** qBittorrent and Transmission are configured to allow plaintext TCP. Fixtures include v1 and hybrid torrents.
- **[T5] Developer tools and depcheck.**
  - Dev tools run via `go run pkg@version` or as installed binaries, never through a `tool` directive in `go.mod`.
  - `tools/depcheck` derives the allowlist from `go mod graph`: stdlib, `golang.org/x/*`, `modernc.org/*` and modules reachable only through `modernc.org/sqlite`.
  - It also checks an explicit denylist of known BitTorrent modules.

### 16.11 Shutdown Order (amends §6.2)

- **[F12, ADR-0015] Concurrent start.** On SIGINT/SIGTERM, the graceful HTTP shutdown (§6.2 step 1, up to 5 s) and the `stopped` announces (§6.2 step 2, in parallel, 3 s timeout) **start at the same time**.
- **The budget is measured from the signal.** The 3-second announce budget starts at the signal, so an in-flight HTTP request or a long-lived SSE stream can never delay the `stopped` announces.
- **Steps 3 and 4 run last.** Flushing stats, bitfields and the DHT table, then closing peers and files and finally the DB, happen only after both the HTTP shutdown and the announces have finished or timed out.
- **Total shutdown time** is bounded by roughly max(5 s, 3 s) plus the time to flush.
- **Verified by** TRK-01 and holdout H12 (`harness/acceptance/holdout/m3`): a `stopped` announce within 3.5 s of SIGTERM while an HTTP request is in flight.

### 16.12 Interpretations Adopted from M1–M3 Implementation Notes (F5)

These clarifications were recorded by implementer agents in `PROGRESS.md`, then reviewed and adopted on 2026-11-08. They are binding.

- **HTTP:**
  - A known route with an unsupported method → 405 `invalid_request` with `Allow`.
  - In `--dev` mode, non-API paths → 404 JSON.
  - A `Host` header without a port means port 80.
- **Bencode (A1):**
  - Lenient mode accepts unsorted or duplicate keys (last wins), leading zeros and `-0`. It still rejects empty integers, non-digits, truncation, non-string keys and trailing data.
  - Integer overflow is an error.
  - `bool` encodes as `i0e`/`i1e`. `[N]byte` decodes only from an N-byte string.
- **Metainfo:**
  - Mistyped top-level `announce`/`announce-list` fields are ignored; only the info dict decides validity.
  - Tracker tiers keep file order. Blank and duplicate URLs and empty tiers are dropped, and tiers stay contiguous. `announce` is tier 0 only if `announce-list` yields nothing. URLs are not validated.
  - Zero-length files are allowed. A torrent whose visible total is 0, or whose total overflows, is rejected.
  - Pad files are validated like any file, count toward piece count and offsets, and are excluded from duplicate detection, `files[]` and `sizeBytes`.
  - v2-only means "no `pieces` and `meta version` ≥ 2".
  - The name is validated and sanitized like a path component.
  - A reserved device stem gets `_` appended (`CON` → `CON_`).
  - Duplicate detection runs on the sanitized, NFC-normalized, case-folded path.
- **Uploads:** the `file` part may be at most 10 MiB, and the whole body 1 MiB more. Either limit → 413. A body that isn't multipart, or has no `file` part → 400 `invalid_request`.
- **Engine defaults:**
  - Without `--peer-port`, the peer port is the `listen_port` setting, otherwise 42069. A bind failure logs a warning (G16).
  - Without `downloadDir`, the download dir is `default_download_dir`, otherwise `~/Downloads`.
  - Blocks that arrive when they aren't wanted count in `downloadedTotal` and `wastedTotal`.
  - A `magnet` field without a file → 400 `invalid_request` until M5.
