# PROGRESS — torrentd

## Status (2026-11-08): milestone 4 (swarm engine), Approach A+
`harness/factory gate --upto 4`: 79/80. M1 18/18, M2 21/21, M3 15/15, M4 25/26, including SEED-02 against libtorrent in Docker and RACE-01.
The one gap is AC-06, which is missing: its test package `./tools/swarm` doesn't exist (escalated).
Coverage: piece 98.3%, choke 95.5%, storage 93.1%, peer 97.9%, session 84.0%, btest 93.9%, store 83.7%, api 93.7%.

## Milestone 4 (swarm engine), Approach A+ (single implementer on frozen contracts)
I built the work packages WP1–WP7 from `spec/plans/m4-work-packages.md` in order, each against its frozen contract tests.

- `internal/bt/piece`: the picker was rewritten (`picker.go`, `pick.go`, `layout.go`).
  - Per-block owners with request times, plus a "released" list so late answers are still accepted.
  - Fresh pieces live in per-class availability buckets (indexed sets, with a random start for tie-breaks). Started pieces are in a per-class active set.
  - Rules 2–5 follow ADR-0018. After a hash failure, a piece is exclusive to one peer.
  - Expiry uses a FIFO of requests (lazy invalidation).
  - O(1) `Interesting` and verified-count. No per-call scan of all pieces (`TestContractPickerCost`).
- `internal/bt/choke`:
  - The choker: ranked by not-snubbed, then rate, then current holder, then random. Optimistic peer kept for 30 s. `Fill` keeps the existing slots.
  - The meter: a 250 ms-slot ring over 5 s.
- `internal/bt/storage`:
  - A FIFO worker `Pool`; the storage runs its work on the shared `Pools`, with private pools when none are given.
  - Close cancels queued work (it gets ErrClosed).
  - `VerifyPieceAsync` hashes on a hash worker, reading the files directly with a 256 KiB buffer.
  - `Sync` fsyncs every file written since the last Sync, reopening evicted ones. `Stat` and `PlanResume` (pure).
- `internal/bt/peer`: `AcceptHandshake`, and `OnPieceSent` called after the flush that wrote the piece.
- `internal/store`: `torrents.go` (Load/SaveTorrents in one transaction, bans). Times are RFC 3339 UTC.
- `internal/bt/session`: rewritten.
  - `session.go`: listener, incoming handshakes (≤ 64 at once), bans and strikes, actions, Close.
  - `torrent.go`: actor, lifecycle, checking, publish.
  - `peers.go`: connections, the download data path, choking, uploads.
  - `pieces.go`: writes, hashing, verification.
  - `persist.go`: flush.
- Data path, as in ADR-0017:
  - The read goroutine sends `evBlock` and waits on a per-peer reply channel. If the block is accepted, it calls `WriteBlock` itself and then reports `evWritten`.
  - `VerifyPieceAsync` runs once a pending piece's writes are done. Its callback calls `MarkVerified` and sends `evVerified`.
  - Uploads: one helper goroutine per peer, fed through a channel with capacity `UploadReadAhead`. `OnPieceSent` counts `uploadedTotal` atomically and does a non-blocking wake of the actor; the tick also pumps the queues.
- Persistence:
  - The actor publishes a `persistSnap` (the record) with every Status.
  - The flusher runs every 10 s, and on request after each state change. It does Sync → Stat → one SaveTorrents.
  - `Close`: stop the flusher → actors shut down (stopped announces in parallel with closing peers) → final flush → close storages → close pools.
- `internal/api`: lifecycle routes (204 / 404 / 409); `stopReason` and `checkingProgress` in the JSON.
- `cmd/torrentd`: the store is the session's Store, and `PreviousClean` comes from `BeginSession`.

### Interpretations (M4)
- **StartPaused:** Add returns `paused` (the frozen test wants this). The local check still runs, but it is reported as `paused` (`hideCheck`). See `escalations/M4-contract-startpaused-initial-state.md`.
- **Stale requests:** a peer may never answer some pieces (RES-04's StallFrom peer).
  - Requests outstanding for longer than max(1 s, 4 × the peer's smoothed latency) don't count against its pipeline depth.
  - The total stays capped at 250, and every tick tops up all pipelines.
  - Expiry, which takes the blocks back after 20 s, is unchanged.
- **Redial backoff:** none for peers we close ourselves on pause or stop. The backoff map is cleared on teardown, so resume redials at once (PAUSE-01).
- **Resume from error:** a full recheck. The picker is rebuilt, and every piece overlapping an existing file is hashed.
- **Restored `paused` + complete:** goes to stopped/user, the same as a StartPaused add over complete data.
- **Bans:** strikes and bans are process-wide. The ban is persisted in a tracked goroutine, so the actor never blocks on the DB. Other torrents get an `evBanned` event.
- **LINT-01 at base:** the architect's `internal/bt/btest/btest.go` used `time.Now`/`time.Sleep` directly, which the T1 lint forbids under `internal/bt/**`. It now uses `clock.Real()`.
- **AC-06:** its test package `./tools/swarm` doesn't exist, and I can't create a counting test. See `escalations/M4-ac06-missing-test-package.md`.

## Status (2026-11-07): milestone 3 (single-peer download), Approach C
`harness/factory gate --upto 3`: 54/54 pass (M1 18, M2 21, M3 15), including
SP-01 against the libtorrent seeder in Docker. All 15 M3 checks passed on
the first gate run after integration, and again on a repeat run.
Coverage: peer 97.7%, piece 100%, storage 97.6%, tracker 97.8%,
session 95.0%, api 92.8%.

## Milestone 3: done (Approach C: contract-first fan-out)
Four work packages were built in parallel against frozen `contract.go`
files (`impl/m3-c-{tracker,peer,piece,storage}`), then merged with
`--no-ff` and no conflicts. Implementer notes are in `notes/<wp>.md`.

- `internal/bt/tracker`, `peer`, `piece`, `storage`: as per their contracts
  and `notes/`. Not changed at integration.
- `internal/bt/session` (integration): `session.go` (New, Add, List, Get,
  Close, peer listener), `torrent.go` (the actor, storage events, snapshots,
  shutdown), `peers.go` (dialling, wire messages, request pipeline).
  - One actor goroutine per torrent owns the picker, peers and state. Dial
    goroutines, `peer.Conn` callbacks, storage jobs and the tracker loop only
    send events. Counters are atomics so the tracker's `Stats` callback can
    read them. A Status snapshot is published after every event.
  - The actor calls `peer.NewConn` itself when a dial goroutine reports a
    handshaken connection, so the peer is registered before any of its
    messages can reach the actor.
  - Block writes run in goroutines; a complete piece is hashed only once
    every write of that piece has finished (writes of one piece can finish
    out of order). `VerifyPiece` + `MarkVerified` run in one job.
  - Limits: 50 peers, 20 half-open dials, a 10 s dial + handshake timeout
    (`context.WithTimeout`, which the T1 lint allows). Duplicates and our own
    listen address are skipped. Pipeline: `PipelineDepth(rate)` with rate =
    the peer's average since it connected; every unchoked peer is topped up
    after each block, choke, verify failure and disconnect.
  - Close: each actor stops its tracker loop (stopped announce, 3 s) in
    parallel with closing peers, waiting for jobs and closing storage.
- `internal/api`: the M3 route table from `contract.go` (`routeTorrents`),
  `POST/GET /api/v1/torrents`, `GET /api/v1/torrents/{id}`, `NewTorrent`.
  `readUploadForm` generalizes the preview upload reader to keep the other
  form fields (each at most 64 KiB).
- `cmd/torrentd`: `engine.go` creates the session. HTTP shutdown and
  `Session.Close` run at the same time on SIGTERM (ADR-0015 §3), then the
  clean-shutdown marker and the DB close.

### Interpretations (M3)
- **Peer port without `--peer-port`:** the `listen_port` setting, otherwise
  the §7 default 42069. A bind failure only logs a warning (G16).
- **Default download dir:** the `default_download_dir` setting (a
  JSON string, absolute), otherwise `~/Downloads`.
- **State after Close:** the last Status is kept as it was (no switch to
  `stopped`); there is no persistence before M4.
- **Seeding at M3:** on completion we send `not interested` to every peer
  and keep the connections. We send `have` for every verified piece.
  Incoming connections are accepted and closed.
- **Blocks received while not downloading** (after completion) are counted
  in `downloadedTotal` and `wastedTotal`.
- **Paused** (`startPaused`) torrents don't open storage or announce, and
  nothing can resume them before M6.
- **A `magnet` field without a file** → 400 `invalid_request` (M5 adds
  magnets). An empty `startPaused` is treated as absent.

## Milestone 2 status (2026-11-07, Approach A)
`harness/factory gate --upto 2`: see the final report / latest `runs/gate/`.
All 21 M2 entries passed on the first gate run after the implementation.
Coverage: bencode 97.5%, metainfo 100%, api 99.5%.

## Milestone 2: done
- `internal/bt/bencode`: reflection-based `Marshal` (canonical: map keys and
  struct fields sorted bytewise), `Unmarshal` (lenient) and
  `UnmarshalStrict`, `RawMessage`, struct tags, `Valid`. Depth cap 64
  (checked before recursing, so a million `l`s can't overflow the stack);
  string lengths are checked against the remaining input before any
  allocation, digit by digit, so huge lengths can't overflow either.
- `internal/bt/metainfo`: `Parse` (whole file, lenient) and `ParseInfo`
  (info dict, strict; reusable for ut_metadata in M5). `ParseMagnet` per the
  pinned contract.
- `POST /api/v1/torrents/preview` in `internal/api/preview.go`.
- Dependency added: `golang.org/x/text` (NFC normalization and case folding
  for A3 duplicate detection). Allowed by §2.

## Milestone 2: decisions and interpretations
- **Lenient mode** accepts everything strict mode rejects *for canonicity*:
  unsorted and duplicate keys (last wins), leading zeros in integers and
  lengths, and `-0`. It still rejects empty integers (`ie`, `i-e`),
  non-digits, truncation, non-string keys and trailing data. The contract
  says strict "additionally" rejects those, so this is the literal reading.
- **Integer overflow** (beyond int64, or beyond the target type) is an error.
- **bool** encodes as `i0e`/`i1e` and decodes from any integer (non-zero is
  true). Byte arrays (`[20]byte`) encode as strings and decode only from a
  string of exactly that length. Nil pointers, nil interfaces and empty
  `RawMessage`s are always left out of a struct (they have no encoding);
  at top level or in a list they are an error.
- **Top-level tracker fields** (`announce`, `announce-list`) are decoded
  tolerantly: if they are mistyped they are ignored, not fatal. Only the info
  dict decides whether a torrent is valid.
- **Trackers:** announce-list tiers in file order; blank URLs and URLs seen
  in an earlier position are dropped; empty tiers are dropped and the tier
  numbers stay contiguous. `announce` is used as tier 0 only when the
  announce-list yields nothing. URLs are not validated (libtorrent's
  `sample.torrent` has a scheme-less one, which the oracle keeps).
- **Sizes:** individual zero-length files are allowed; a torrent whose
  visible (non-pad) total is 0 is rejected, as is a single-file `length <= 0`.
  Total size overflow is rejected.
- **Pad files** (`attr` contains `p`) are validated like any file (a pad
  without a path is rejected), count towards the piece count and offsets, and
  are excluded from duplicate detection (hybrid torrents repeat `.pad/N`),
  `files[]` and `sizeBytes`.
- **v2-only** is detected as "no `pieces` and `meta version` ≥ 2"; any other
  torrent without `pieces` is rejected as missing pieces. Both are
  `invalid_torrent`.
- **Name** is validated as a path component (no `/`, `\`, `.`, `..`, empty)
  and sanitized like one. `name.utf-8`/`path.utf-8` win when present and
  non-empty.
- **Sanitizing (A3):** `<>:"|?*`, control characters and invalid UTF-8 → `_`;
  each trailing dot/space → `_`; a reserved device stem (CON, PRN, AUX, NUL,
  COM0–9, LPT0–9, case-insensitive, before the first dot) gets `_` appended
  (`CON` → `CON_`, `nul.txt` → `nul_.txt`). Duplicate detection runs on the
  sanitized path, NFC-normalized and case-folded, so `a?` and `a*` collide.
- **Piece count** above 2²¹ (T6) is rejected.
- **Non-power-of-two piece length** is logged at INFO by the preview handler.
- **Upload limit:** the `file` part may be at most 10 MiB (10 485 760 bytes);
  the whole body may be 1 MiB larger for multipart overhead. Either limit →
  413 `payload_too_large`. A non-multipart body, a body without a `file` part
  or a malformed multipart body → 400 `invalid_request`. Other parts are
  skipped.
- **GET /api/v1/torrents/preview** → 405 `invalid_request` with `Allow: POST`
  (the M1 wrong-method convention).

## Milestone 1 (merged)
- `go.mod`, `internal/{config,store,clock,api}`, `cmd/torrentd`,
  `webembed.go`, `tools/depcheck`, `Makefile`. See git history and
  `retros/2026-11-07-approach-a-m1/` for details and M1 decisions (405 with
  `invalid_request`, `--dev` 404 JSON, Host without port = port 80, origins).

## Next
- M4: seeding (choking, uploads), resume and persistence, random-first and
  endgame (see `escalations/M3-piece-order-vs-random-first.md`).
