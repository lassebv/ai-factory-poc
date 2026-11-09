# PROGRESS — torrentd

## Status (2026-11-08): milestone 4 (swarm engine), Approach A
- All 25 runnable M4 checks pass when run individually: SEED-01/02 (libtorrent in Docker), SWARM-01, CHOKE-01, WIRE-03, PICK-01/02, PIPE-01, BAN-01, POISON-01, CPU-01, RES-01..07, STOP-01, PAUSE-01, LIFE-04, SHUT-01, MEM-01, FD-01 and RACE-01.
- **AC-06 is `missing`:** `./tools/swarm` doesn't exist and its test would be verifier-owned. See `escalations/M4-ac06-tools-swarm-missing.md`. Because of this, `gate --upto 4` cannot exit 0.
- Coverage: session 86.9%, piece 97.9%, storage 96.0%, store 87.5%, api 92.9%.
- Contract changes are recorded in ADR-0017.

## Milestone 4: what was built
- **piece:** a new picker.
  - Pick order: piece affinity, then new pieces by priority class (random-first for the first 4, then rarest-first with random ties), then other peers' in-progress pieces, then endgame duplicates.
  - `Received` only accepts blocks outstanding for that peer, and returns the peers to cancel.
  - `Release` returns the released blocks. Also new: `Discard`, `SetPriority`, `Endgame`, `HaveCount`.
  - Pick is a single scan with a bounded candidate set, with no full sort.
- **storage:** `Sync()` fsyncs dirty files.
- **session:**
  - `pool.go`: process-wide disk and hash pools, non-blocking submit.
  - `incoming.go`: accept loop and incoming handshakes, 10 s deadline, ban and accepting checks.
  - `torrent.go` + `checking.go`: the actor, lifecycle (halt, command, fail, finish), open/check/resume, hash results, strikes and bans, snapshots and the final record.
  - `peers.go`: dialling, the download side, the pipeline (depth from max(smoothed rate, average since connect)), request timeout and snubbing.
  - `upload.go`: serving requests through the disk pool, the choker (4 regular slots plus 1 optimistic, filled immediately; 10 s rounds; rotation every 30 s), and the 1 s tick.
  - `resume.go`: per-file size/mtime fingerprint, the missing-files check, `wantedBytes`.
  - `session.go`: Load/Add, lifecycle dispatch, strikes and bans (persisted), the flusher (every 10 s, on Add and after actions, Sync before saving), and Close (flusher stop → actors → pools → final save).
- **store:** `torrents.go` implements `session.Store` (torrents, trackers, banned_peers).
- **api:** the pause/resume/stop-seeding routes and `stopReason`.
- **cmd:** `startEngine` passes the store and the clean flag, and calls `Load`.

### Interpretations (M4)
- **Data on add:** if any file already has data on disk, adding the torrent triggers a full check. Otherwise checking only opens storage.
- **Restored torrents:** paused and stopped torrents don't open storage until they are resumed. Their progress comes from the saved bitfield.
- **Fast resume:** fingerprint mismatch, or an unclean shutdown with any size mismatch, triggers a full recheck, and all saved pieces are forgotten. A torrent with an empty saved bitfield is never checked.
- **Pause:** allowed in checking and downloading. It's a no-op when already paused, and `invalid_state` on a complete torrent, in error or in stopped.
- **Stop seeding:** a no-op when stopped. `not_complete` when incomplete. `invalid_state` in checking or error with complete data.
- **Resume in error:** retries with a full recheck. For "Files missing" it goes back to error while the files are still gone.
- **Seeding torrents don't dial out.** On completion, connected seeds are dropped.
- **Oversize requests** (more than 16 KiB) disconnect the peer; there's no BEP 6 yet. Invalid requests also disconnect. Requests from choked peers are ignored.
- **Ban side effects:** a ban closes that IP's peers in every torrent and discards in-progress pieces it contributed to. Pieces that are pending hashing still fail and strike.
- **Stale requests:** a request unanswered for 3 s, or skipped because the peer already answered a later one, no longer counts against the pipeline depth. RES-04 failed about 1 time in 2 under load without this: the fake peer ignores requests for pieces ≥ 32, and random/rarest order clogged the pipeline with dead requests.
- **Backlog limit:** at most 512 received blocks wait for the disk. Above that, `fill` pauses until the backlog has halved.

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
