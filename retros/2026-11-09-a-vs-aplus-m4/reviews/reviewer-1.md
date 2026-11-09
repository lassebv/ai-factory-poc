# Blind comparative review: torrentd milestone 4 (X vs Y)

Both trees build and pass `go vet`. Both pass `go test -race -count=1 ./...` with no races reported. Per-package coverage is similar and above the gate (`session` 84.0% for X, 87.1% for Y). No file is over 800 lines. Neither tree calls `time.Now/After/Tick/NewTimer/NewTicker` outside tests in `internal/bt` or `internal/store`, so both honour T1. Below are the differences in the M4 code.

## X

**Architecture.** X follows the §4.4 layout, including a separate `internal/bt/choke` package. It has a pure `Choker` with `Rechoke` and `Fill` (`choke/choke.go:113-186`) and a mutex-protected sliding-window `Meter` that uses the injected clock (`choke/meter.go`).

The process-wide pools live in the storage layer:
- `storage.Pools` is passed in through `Options.Pools` (`storage/contract.go:160-186`).
- Every `WriteBlock`, `ReadBlock` and `Sync` goes through `onDisk` (`storage/m4.go:30`).
- Every `VerifyPiece` goes through the hash pool (`storage/storage.go:319`).

So the bound cannot be bypassed. The session API takes a `context` throughout (`session/contract.go:357-360`).

**Picker.** The picker (`piece/picker.go`, `pick.go`) is the strongest part of either submission:
- availability buckets per priority class, so it never scans every piece;
- per-request expiry through a time-ordered queue (`picker.go:382-419`);
- late answers accepted from peers whose requests were released (`released`);
- a cooldown for peers that time out;
- "exclusive" re-download after a hash failure, which isolates the culprit;
- contributor IPs tracked per piece, returned by `PieceFailed` (`picker.go:563`).

**Data path.** Backpressure is per peer. The read goroutine asks the actor about each block, then writes it itself through the disk pool (`session/peers.go:207-226`).

**Resume.** Fast resume is finer-grained than Y's. `planResume` only rechecks pieces of files whose fingerprint doesn't match, uses sizes only after an unclean shutdown, and implements G8 for missing files (`storage/m4.go:149-238`).

**Persistence.** Snapshots are lock-free (`persist atomic.Pointer`), and the flusher fsyncs before it saves the bitfield (`session/persist.go:63-72`).

**Shutdown.** Shutdown keeps draining actor events while peers and helpers finish (`torrent.go:696-724`), so it avoids deadlocking with read goroutines that are waiting for a block decision.

**Bounds.** Incoming handshakes are limited by a semaphore (`session.go`, `acceptSem`).

**Weaknesses:**
- **Storage leak:** A stale `evChecked` stores `ev.st` before the generation check (`torrent.go:583-588`). Pausing during the first check, before storage exists, and then resuming opens a second Storage, and the first one is never closed.
- **Pieces sent after choke:** Blocks already handed to the uploader are still sent after we choke the peer. `rechoke` only clears `p.uploads` (`peers.go:473`, `201`).
- **Partial pieces kept after a ban:** `dropIP` (`peers.go:258`) doesn't throw away partial pieces the banned IP contributed to.
- **Coupling:** `session` imports `internal/store` (`torrent.go:21`), and `SaveTorrents` deletes every tracker row, which will also wipe user-added trackers later (`store/torrents.go:203`).
- **State masking:** The `hideCheck` hack reports `checking` as `paused` (`torrent.go:767`).

**Tests.** Testing is solid:
- an in-repo goroutine-leak checker (`btest/btest.go`);
- choker and picker tests driven by the fake clock;
- M4 contract tests for seeding, lifecycle, persistence, missing files, banning a corrupt peer and stalled peers.

## Y

**Architecture and readability.** Y is compact and easy to read. Its actor (`session/torrent.go`) is split cleanly across `checking.go`, `upload.go`, `resume.go`, `incoming.go` and `pool.go`.

Its persistence direction is cleaner than X's: `session` defines `Record` and `Store`, and `store` adapts to them (`store/torrents.go:17`). `SaveTorrents` keeps user-added trackers (`:79`).

**Ban handling.** Ban handling is good. Contributors are recorded per piece (`peers.go:317-322`). On a ban, the partial pieces that IP contributed to are thrown away (`torrent.go:612-617`), and the ban is sent to the other torrents and saved off the actor goroutine.

**Persistence and shutdown.** The flusher takes the snapshot before calling `Sync`, so pieces are fsynced before their bitfield is saved (`session.go:405-417`). On shutdown, storage is synced and the fingerprint is taken after `Close` (`torrent.go:688-695`).

**Weaknesses:**
- **Layout and context:** There is no `choke/` package; choking lives in `session/upload.go` (§4.4). The lifecycle API has no `ctx` (`contract.go:324`).
- **Hashing not cancelled on pause:** A recheck queues every piece on the hash pool, and each job checks only `t.ctx`, never the generation (`checking.go:119-128`). Pausing during a check doesn't stop the hashing. Resuming queues a second full pass.
- **No limit on incoming handshakes:** Every accepted connection gets its own goroutine (`incoming.go:38-41`).
- **Hot-path copies and scans:** `picker.Have()` clones the bitfield on every upload request (`upload.go:34`). `updateInterest` scans every piece on every `have` (`peers.go:279-286`). `Pick` scans every piece and re-sorts the active set on every call (`picker.go:227-230`, `282`).
- **Coarse request expiry:** Expiry releases a peer's whole pipeline after 30 s. It then resets `waitSince`, so a peer is only marked snubbed after about 90 s (`peers.go:422-432`).
- **Unbounded hash queue:** The hash pool's queue has no bound, so a recheck can queue up to 2²¹ jobs at once.
- **Checking progress:** `checkingProgress` is hard-coded to 0 (`api/torrents.go:216`).
- **No generation on hash results:** `evVerified` doesn't carry a generation.

**Tests.** The actor tests are white-box and useful, but they drive timeouts with `time.Now()` instead of a fake clock (`actor_test.go:250`). They also rely on `waitFor` and `drainEvents` with real time.

## Preference

I would rather maintain X. Its pools are enforced at the storage layer, its choker is a separate, testable package, and its picker already handles per-request expiry, exclusive re-downloads and contributor tracking, so it holds up better for queueing, limits and stats in later milestones. Y reads more easily and its persistence boundary is cleaner, but it leaks work on pause, doesn't limit incoming handshakes, copies state in hot paths, and departs from the §4.4 layout.

```json
{"X": {"correctness": 7, "architecture": 9, "design": 8, "readability": 7, "tests": 8, "performance": 8, "security": 8,
       "defects": [
         {"where": "X/internal/bt/session/torrent.go:583", "severity": "medium", "what": "Stale evChecked assigns ev.st to t.store before the checkGen test; pause during the first check (store nil) then resume opens a second Storage and the first is overwritten and never closed (fd/handle leak, two Storages on the same files)."},
         {"where": "X/internal/bt/session/peers.go:473", "severity": "low", "what": "Choking a peer clears only p.uploads; blocks already in readCh/inflight are still read and sent by uploader (peers.go:201) after the choke message."},
         {"where": "X/internal/bt/session/peers.go:258", "severity": "low", "what": "dropIP on a ban does not discard in-progress pieces the banned IP contributed to, so they likely fail again and give innocent co-contributors strikes."},
         {"where": "X/internal/bt/session/torrent.go:767", "severity": "low", "what": "hideCheck reports state checking as paused to satisfy a test, so Status can disagree with the real actor state."},
         {"where": "X/internal/store/torrents.go:203", "severity": "low", "what": "SaveTorrents deletes all tracker rows on every flush (user-added trackers will be wiped later); session also imports internal/store directly (torrent.go:21), coupling the engine to persistence types."}
       ]},
 "Y": {"correctness": 7, "architecture": 6, "design": 7, "readability": 8, "tests": 7, "performance": 6, "security": 6,
       "defects": [
         {"where": "Y/internal/bt/session/checking.go:119", "severity": "medium", "what": "Recheck queues one hash job per piece that checks only t.ctx (never cancelled by pause); pausing mid-check keeps hashing the whole torrent, and resume queues a second full pass. The hash queue is unbounded (up to 2^21 jobs)."},
         {"where": "Y/internal/bt/session/incoming.go:41", "severity": "medium", "what": "Each accepted connection gets its own handshake goroutine with no concurrency limit, so a connection flood creates unbounded goroutines and open sockets (10 s each)."},
         {"where": "Y/internal/bt/session/upload.go:34", "severity": "medium", "what": "picker.Have() clones the full bitfield on every incoming request; updateInterest (peers.go:279) scans every piece on every have; Pick re-sorts the active set and scans every piece per call."},
         {"where": "Y/internal/bt/session/peers.go:426", "severity": "low", "what": "Request expiry is all-or-nothing per peer after 30 s, and the waitSince reset delays snub detection to about 90 s instead of 60 s."},
         {"where": "Y/internal/api/torrents.go:216", "severity": "low", "what": "checkingProgress is hard-coded to 0; there is no choke/ package (§4.4) and the lifecycle API lacks context (session/contract.go:324)."}
       ]},
 "prefer": "X", "why": "X enforces the process-wide disk and hash pools inside storage, keeps choking in its own testable package, and has a much more robust picker (per-request expiry, exclusive re-download, contributor tracking); Y is cleaner to read but leaks work on pause, has no limit on incoming handshakes, copies state in hot paths and departs from the §4.4 layout."}
```
