# ADR-0017: Milestone 4 contract evolution (picker, storage, session, API)

- **Status:** Accepted (2026-11-08, Approach A single agent holding the architect role)
- **Spec:** §4.1, §4.5, §5, §6, §7, §16.2 A5/G10/T6, §16.3 C3/G8 · **Builds on:** ADR-0011–0016

## Context

The M3 `contract.go` files describe a single-peer leecher. M4 (§15.4, ADR-0016) adds seeding, choking, the full picker, banning, persistence and the lifecycle actions. Four known defects of the M3 engine were contract-level: `Received` accepted unrequested blocks, there was no endgame or request timeout, `Pick` sorted every piece per call, and disk/hash work was one goroutine per block behind per-torrent semaphores.

## Decision

### `internal/bt/piece`
- `Received(p, b)` now returns `(accepted, pieceDone bool, cancel []PeerKey)`. A block is accepted **only if it is outstanding for p** (POISON-01). `cancel` lists the other peers it was outstanding for (endgame duplicates).
- `Release(p)` returns the released blocks, so the caller can send `cancel`.
- New: `Priority` classes (`Skip < Low < Normal < High`, default `Normal`), `SetPriority`, `Discard(index)` (drop received blocks of an in-progress piece, used after a ban), `Endgame()`, `HaveCount()`. `Done()` means every *wanted* piece is have.
- Pick order: (1) in-progress pieces nobody else works on (piece affinity, which limits innocent strikes under ADR-0006), (2) new pieces by priority class, random-first until 4 pieces are verified, then rarest-first with random ties, (3) the rest of other peers' in-progress pieces, (4) endgame duplicates once every wanted block has been requested (A5). With a nil random source the picker is deterministic (no random-first, ties by index), so the M3 order tests still hold. `Pick` keeps a bounded candidate set in one scan and does not sort every piece.

### `internal/bt/storage`
- New `Sync()`: fsyncs every file written since the last Sync. The session calls it before saving a bitfield (§16.3 C3).
- `Open` still treats every piece as unverified. A caller restoring resume data calls `MarkVerified` for each trusted piece, which also renames completed `.part` files.
- The per-Storage worker limits stay as a safety net. The §4.5 process-wide pools live in the session.

### `internal/bt/session`
- `Config` gains `Store`, `PreviousShutdownClean`, `DiskWorkers` and `HashWorkers`.
- The `Store` interface, `Record` and `Ban` types are defined here and implemented by `internal/store` (`(*store.Store).Torrents()`). The session never imports the store package.
- `Session` gains `Load`, `Pause`, `Resume` and `StopSeeding`. New errors: `ErrNotFound`, `ErrInvalidState`, `ErrNotComplete`. `Status` gains `StopReason`, and `UploadedTotal` is now filled.
- Constants: `FlushInterval` (10 s), `BanStrikes` (3), `BanDuration` (24 h), `FilesMissingMessage`, the `Desired*` states and the `StopReason*` values.
- Engineering choices pinned in the contract doc:
  - process-wide disk (4) and hash (GOMAXPROCS/2) pools with non-blocking submit;
  - at most 512 received blocks waiting for the disk, after which the pipeline pauses;
  - request timeout 30 s (release, cancel, then a depth of 1 until the peer delivers);
  - stale requests: a request stops counting against the pipeline depth after 3 s, or at once when the peer answers a later request (peers answer in order, so it was skipped). It stays owned by that peer, so endgame can still duplicate it. Live plus stale requests are capped at 2 × 250 per peer. Without this, a peer that ignores some requests clogs the pipeline (found by RES-04 under load);
  - snubbing at 60 s;
  - seeding torrents don't dial;
  - a request above 16 KiB closes the connection, because there's no fast extension yet.

### `internal/api`
- `TorrentService` gains `Pause`, `Resume` and `StopSeeding`.
- Routes: `POST /api/v1/torrents/{id}/{pause,resume,stop-seeding}` → 204. Errors: 404 `not_found`, 409 `invalid_state`, 409 `not_complete`. Other methods on those paths → 405 with `Allow: POST`.
- `stopReason` is mapped in state `stopped`.

## Consequences

- Contract tests were updated where M3 semantics changed: unrequested blocks are now rejected, and the endgame hands out duplicates once everything is requested. The API contract tests cover the action routes.
- `Record`/`Store` will grow in M6 (seeding seconds, limits, queue). That is additive.
- Fast resume compares per-file `{size, mtimeNs}` JSON. After an unclean shutdown only sizes are compared (ADR-0002).
