# PROGRESS — torrentd, Approach A

## Status (2026-11-07): milestone 3 (single-peer download)
All 15 M3 checks pass, including SP-01 against libtorrent in Docker. The
earlier milestones still pass (`harness/factory gate --upto 3`).
Coverage: peer 93%, piece 98%, session 90%, storage 97%, tracker 98%,
api 99%.

## Milestone 3: what was built
- `internal/bt/tracker`: HTTP(S) announce (`http.go`). Builds the URL (raw
  info_hash/peer_id percent-encoded, `compact=1`, `numwant=50`, `key`,
  `event`, `trackerid`) and parses the response with lenient bencode:
  compact and dict `peers`, `peers6`, intervals, `failure reason`,
  `warning message`, `tracker id`, complete/incomplete. `tiers.go` implements
  BEP 12: tiers in order, shuffled once with the injected rng, and a
  responding tracker moves to the front of its tier. Non-HTTP URLs are
  skipped (UDP is M5).
- `internal/bt/peer`: handshake, message codec, and `Conn` (separate read
  and write goroutines, a non-blocking `Send` with a bounded queue,
  keep-alives after 90 s of silence, a 3 min idle read timeout). Messages
  longer than 16 KiB + 13 bytes on the wire are `ErrTooLarge`; the only
  exception is a bitfield up to the torrent's exact bitfield length. A
  malformed payload is `ErrMalformed`. Unknown IDs are passed through and
  ignored.
- `internal/bt/piece`: `Bitfield` and `Picker` (16 KiB blocks, picking
  in-progress pieces first, then rarest-first, then endgame duplicates once
  every missing block has been requested).
- `internal/bt/storage`: maps pieces onto files and preallocates them sparsely
  as `.part` (empty files are created under their final name). A file is
  renamed when all of its pieces are verified. Pad files never touch the disk
  and read as zeros. A handle LRU (128) and an unbounded-queue worker `Pool`
  (disk 4, hash GOMAXPROCS/2).
- `internal/bt/session`: `Session` (binds `--peer-port` on all interfaces;
  incoming connections are accepted and closed per ADR-0011; peer ID
  `-TD0100-` + 12 random bytes; one random key per session). One `Torrent`
  actor per torrent, with an announcer goroutine.
  - Blocks are buffered in memory per piece, hashed on the hash pool, then
    written on the disk pool. Corrupt data never reaches the disk.
  - Pipeline depth per D1 (`clamp(rate×2s/16KiB, 5, 250)`).
  - At most 50 peers per torrent and 20 half-open connections.
  - Reconnect backoff of 5 s × 2ⁿ, capped at 5 min.
- `internal/api`: `POST /api/v1/torrents`, `GET /api/v1/torrents` and
  `GET /api/v1/torrents/{id}` (the Torrent schema, per ADR-0011). The
  method-less preview route was replaced by `GET …/preview` → 405 plus a
  method-less `…/{id}` fallback that answers 405 (`Allow: POST` for
  `preview`, `GET, HEAD` otherwise).
- `cmd/torrentd`: starts the session before HTTP and closes it after the
  HTTP shutdown. That sends the stopped announces (3 s budget), then closes
  peers and files.

## Milestone 3: decisions and interpretations
- **Piece order (escalated, `escalations/M3-stor02-assumes-in-order-picking.md`):**
  no random-first phase at M3. Rarest-first, ties broken by the lowest
  index, so a single peer is downloaded in order. STOR-02's fake peer serves
  requests FIFO and stalls on piece ≥ 4, so random-first would make it flaky.
- **Bitfield:** one is accepted at any point, not only as the first message.
  It is merged with earlier `have`s. A second bitfield, a bitfield of the
  wrong length or with spare bits set, or a `have` out of range disconnects
  the peer.
- **Wasted bytes:** a failed hash counts the whole piece. Unrequested,
  cancelled and duplicate blocks count their length. `downloadedTotal`
  counts every received block payload.
- **`left`** = total length (pad files included) minus verified piece bytes.
  `completedBytes` counts only non-pad bytes of verified pieces.
- **Announces:** `numwant=50` on every announce, including `stopped`.
  `started` is retried with backoff (15 s × 2ⁿ, capped at 30 min) until it
  succeeds. `completed` is sent as soon as the download finishes (after
  `started` succeeds). `stopped` is sent only if some announce succeeded.
  The interval floor is 30 s.
- **POST /torrents:** `downloadDir` defaults to the `default_download_dir`
  setting, or `~/Downloads` when the setting is unset. The dir is created with
  its parents, and writability is checked with a temp file that is removed
  right away. A non-absolute dir, or one that can't be created or written,
  → 400 `invalid_download_dir`. `startPaused=true` → state `paused`, with no
  network activity. A `magnet` field without a file → 400 `invalid_request`
  (magnets are M5). Unknown form parts are skipped. Known text fields are
  limited to 64 KiB.
- **Duplicate add** → 409 `AlreadyExists` with the id. Merging new trackers
  into the existing torrent is not done at M3 (there is no tracker API yet).
- **Peer port busy at startup (G16):** log an ERROR and fall back to a random
  port, so the UI still starts. When `--peer-port` is unset the default is
  `listen_port`'s 42069.
- **GET /torrents/{id}** returns the Torrent object. The TorrentDetail extras
  are M6.
- **Not at M3:** request timeouts and snubbing (a stalled peer's blocks are
  re-requested from other peers only in endgame), choking and uploads,
  persistence, and announce `ipv6=`.

## Milestone 2 (merged)
Bencode, metainfo, magnet parsing and `/torrents/preview`. See
`retros/2026-11-07-approach-a-m2/` and git history.

## Milestone 1 (merged)
Skeleton: config, store, clock, api, cmd, webembed, depcheck. See
`retros/2026-11-07-approach-a-m1/`.

## Next
- M4: seeding (choking, serving requests, WIRE-03), persistence and
  resume, snubbing and request timeouts, and random-first-4 (see the
  escalation).
