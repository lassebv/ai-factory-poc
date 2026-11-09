# ADR-0019: M4 persistence, fast resume and lifecycle semantics

- **Status:** Accepted (2026-11-08, architect, Approach A+ on M4)
- **Spec:** §5, §6.1 step 4, §6.2, §7, §16.3 C3, G8, G18, ADR-0002, ADR-0004, ADR-0016 · **Acceptance:** RES-01…07, STOP-01, PAUSE-01, LIFE-04, SHUT-01
- **Contracts:** `internal/store/contract.go`, `internal/bt/storage/contract.go` (Stat, PlanResume, Sync), `internal/bt/session/contract.go`, `internal/api/contract.go`

## Decision

1. **The repository layer stays in `store`** and gets typed records: `TorrentRecord` and `Ban`. The session depends on a four-method `session.Store` interface. `*store.Store` implements it, and `btest.MemStore` is the fake. The info dict is stored raw (the `metainfo` column) and rebuilt with `metainfo.ParseInfo`, so M5 magnets fit the same row. Trackers go to the `trackers` table.
2. **The flush protocol** runs every 10 s, on every state change, and at the end of `Close`:
   1. capture the record (bitfield, counters, state);
   2. `storage.Sync`;
   3. `storage.Stat` for the fingerprint;
   4. one `SaveTorrents` transaction for all torrents.

   That order implements ADR-0002's invariant: data is fsynced before a bitfield that contains it is persisted. The final flush comes after peers and disk writes have stopped, so the saved mtimes match what a clean restart will see (RES-02).
3. **Fast resume is a pure function**, `storage.PlanResume`, so every case is table-tested without a session:
   - a clean restart compares size + mtime;
   - an unclean restart compares sizes only (ADR-0002);
   - only pieces of *suspect* files are rechecked;
   - pieces that overlap missing files are downloaded again;
   - G8: a saved bitfield with every file missing gives "Files missing", and `Open` is not called, so nothing is recreated;
   - a fresh add hashes the pieces of any existing files (SEED-01).

   `storage.Stat` never creates anything.
4. **The clean-shutdown marker stays in `cmd/torrentd`.** `store.BeginSession` result → `session.Config.PreviousClean`. `MarkCleanShutdown` is written after `Session.Close` returns, which is after the final flush. Close starts at the same moment as the HTTP drain (§16.11).
5. **Lifecycle semantics**, completing G18 where it is silent (see `escalations/M4-action-validity-gaps.md`):
   - pause on paused, stop-seeding on stopped, and resume on active torrents are no-ops (204);
   - pause on a complete torrent → `invalid_state`;
   - stop-seeding on anything not complete (including `checking`) → `not_complete`;
   - pause or stop-seeding in `error` → `invalid_state`;
   - `StartPaused` over complete data ends in `stopped`/`user`, because `paused` means incomplete (§5.1);
   - every action returns after the new state is published, and after it no request or upload happens (PAUSE-01, STOP-01);
   - pause and stop-seeding send a `stopped` announce without waiting for it.
6. **Bans** are process-wide per IP. Strikes are kept in memory, and bans are persisted with a 24 h expiry and loaded at startup.

## Consequences

- A store failure is logged and retried; it never takes the engine down.
- After `kill -9`, the pieces completed since the last flush are downloaded again (at most about 10 s of progress, RES-05).
- The torrent row is written at `Add` time, before the 201, so a crash right after adding still lists the torrent.
