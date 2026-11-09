I'd rather maintain Y. Its structure fits the spec better and its building blocks are cleaner. X is solid, but its choking, rate tracking and request timeouts are all mixed into the session package.

Both trees build, and `go vet` and `go test -race ./...` pass in both. I only read and ran code; I changed no files. Coverage on the main M4 packages: X has session 87%, piece 98%, storage 96% and store 88%. Y has session 84%, piece 98%, choke 96%, storage 93% and store 84%. Neither has direct `time.Now`, `NewTicker` or similar calls (forbidden by T1) in `internal/bt` or `internal/store`.

## X

**Correctness and robustness.** The torrent actor is careful.
- Every pool job and dial is tracked by `t.wg`.
- Stale results from checking are filtered with `gen` (`checking.go:137`).
- `shutdown` cancels the context before waiting for peers, so nothing can block on a full event channel (`torrent.go:678-686`).
- Data is fsynced before a bitfield is saved: the flusher calls `Sync` after the snapshot and skips the record if it fails (`session.go:409-416`).

The real problems are about limits on hostile or heavy input:
- **Incoming handshakes are unbounded.** `acceptLoop` starts a goroutine for every accepted connection with no cap (`incoming.go:38-41`). Each one holds a file descriptor for up to 10 s, so a connection flood can exhaust descriptors.
- **A recheck can starve other torrents.** `startCheck` queues one hash job per piece (`checking.go:114-130`) into a pool queue with no limit (`pool.go:31-41`). Rechecking a big torrent puts up to 2²¹ closures ahead of every other torrent's piece checks.
- **Any changed file forces a full recheck.** One mismatched fingerprint makes `opened()` reset the picker and rehash every piece (`checking.go:87-91`).
- **A slow peer can keep a block indefinitely.** After its first timeout a "slow" peer is never timed out again (`peers.go:426`). Its single outstanding request stays assigned to it until endgame.

**Architecture.**
- There is no `internal/bt/choke` package, which §4.4 lists. Choking, the optimistic slot and the rate meter (a per-second halving filter) live in `session/upload.go`.
- §4.5 is otherwise followed well. Peer goroutines only send events, and disk and hash work goes through process-wide pools (`session.go:109`).
- Context is not passed everywhere: the lifecycle API is `Pause(id)` without a context (`api/torrents.go`, `session.go:283`).
- The 16 KiB request limit, the bitfield size cap and `MaxPipelineDepth` are handled correctly.
- The picker runs "active pieces nobody else is working on" before choosing new pieces by class (`picker.go:234-246`). A started low-priority piece can therefore be requested before a new high-priority one, which bends A5's strict class order.

**Design.** The pools are private to the session package and their limits depend on caller discipline ("callers bound the amount of queued work"). Resume and fingerprint logic sits in the session package rather than storage. Persistence leaves out `torrent_files`, `seeding_seconds` and `limit_override` (`store/torrents.go:60`), which M6 queueing, limits and file priorities will need.

**Readability.** The files are compact, well named and well commented. The cost is that `torrent.go`, `peers.go` and `upload.go` share a lot of mutable actor state.

**Tests.** There are good swarm, lifecycle and resume end-to-end tests. Some tests in `actor_test.go` mainly add coverage:
- `TestChokerSlotsAndRotation` uses `time.Now()`.
- It only logs (`t.Log`) where it should assert that the optimistic slot rotated.
- It ends with calls that have no assertions (`actor_test.go:142-168`).

The in-repo goroutine-leak helper is not used in the session tests.

**Performance.** `picker.Have()` copies the whole bitfield (`picker.go:603`), and it is called for every incoming request (`upload.go:34`), every `updateInterest` (`peers.go:279`) and every new peer. That can mean up to a 256 KiB allocation per 16 KiB block served. `pickNew` scans every piece on each pick (`picker.go:292`). `publish` allocates a full `Status` and loops over all peers after every event (`torrent.go:296`, `704`).

**Security.** The bitfield length cap, request validation and the per-torrent peer cap are in place. The incoming handshake flood above is the gap.

## Y

**Correctness and robustness.**
- Shutdown keeps handling events while peers and jobs finish, so read goroutines waiting on a block decision can't deadlock (`torrent.go:696-724`).
- The flusher syncs before it fingerprints and saves (`persist.go:63-73`).
- Request expiry lives in the picker with a cooldown and accepts late blocks from released requests (`picker.go:382-476`).

The bugs I found:
- **Storage handles can leak.** If a check is cancelled by Pause and the torrent is resumed while `t.store == nil`, a second check opens a second `Storage` (`torrent.go:398`, `550`). `checked()` then overwrites `t.store` for every result, including stale ones (`torrent.go:583-585`). One storage and its file handles leak, and which one survives depends on timing.
- **Pieces can be sent after we choke.** Choking clears `p.uploads` but not the blocks already handed to the upload helper (`peers.go:471-474`, `194-202`).
- **Saving a ban has no timeout.** `SaveBan` runs with `context.Background()` and is tracked by the session wait group (`session.go:283`), so a stuck database delays `Close`.
- **Paused checks are shown as paused.** The `hideCheck` hack reports a checking torrent as paused to satisfy a test (`torrent.go:67-70`).

**Architecture.**
- Y has the `choke` package with a pure `Choker` and a sliding-window `Meter` (`choke/choke.go`, `choke/meter.go`), plus `btest` with an in-repo goroutine-leak check.
- The pools are process-wide and every storage operation goes through them inside storage itself (`storage/m4.go:17-30`), so the limits cannot be bypassed.
- Incoming handshakes are capped by a semaphore (`session.go` `acceptSem`).
- Context reaches the API (`api/torrents.go`, `session.Pause(ctx, …)`).

It does stray from §4.5's "peer goroutines only send events":
- Disk writes are issued from the peer read goroutine (`peers.go:224`).
- `OnPieceSent` updates torrent counters and meters from the write goroutine (`peers.go:171-179`).
- There is an extra upload goroutine per peer (`peers.go:185`).

All of this is atomic and bounded by the pools, so there is no race, but it is a departure.

**Design.**
- The picker owns hash-failure contributors and returns their IPs from `PieceFailed`, which makes striking simple.
- Fast resume is a pure planner (`planResume`, `m4.go:149`) that rechecks only the pieces of suspect files.
- `store` has its own contract and persists files, seeding seconds and `limit_override`, so it is better prepared for M5–M7.
- Two weak spots: `Add` holds `s.mu` across a database write (`session.go:355`), and saving deletes all trackers, including user-added ones (`store/torrents.go:203`).

**Readability.** Packages are well separated and the contracts are documented. A few comments are confusing (the `start` comment about `s.mu`), and `torrent.go` is 791 lines, just under the 800-line gate.

**Tests.** Y has dedicated contract tests for the choker (including anti-snubbing and fill), the picker (expiry, endgame, contributors, cost), the pools (limits, close, shared use), resume planning and the store. The session tests use the goroutine-leak check, and the stalled-peer and banning end-to-end tests are realistic. Tests use a fixed random seed and the fake clock.

**Performance.** The picker uses availability buckets instead of scanning every piece. There is a cached have-mirror and cached wire bitfield (`pieces.go:18-43`), and per-block events skip `publish`. One cost: each `publish` still rebuilds a `TorrentRecord`.

**Security.** Handshakes are capped, the upload queue and read-ahead are bounded, `Stat` checks paths with `filepath.IsLocal`, and requests over 16 KiB disconnect the peer.

## Which I'd maintain

I'd choose Y. Its package boundaries (`choke`, storage-owned pools, a pure resume planner, a `store` contract) match §4.4 and §4.5 and give M5–M7 clear seams. Its tests check behaviour more thoroughly, including leak checks. Its defects are a few local leaks and smells; X's are structural (choking inside session, unbounded handshake and pool queues, bitfield copies on hot paths) and harder to fix later.

```json
{"X": {"correctness": 7, "architecture": 6, "design": 6, "readability": 7, "tests": 6, "performance": 5, "security": 6,
       "defects": [
         {"where": "X/internal/bt/session/incoming.go:38", "severity": "high", "what": "Accept loop starts a goroutine per incoming connection with no limit; each holds a file descriptor for up to the 10 s handshake timeout, so a connection flood can exhaust descriptors and goroutines."},
         {"where": "X/internal/bt/session/checking.go:118", "severity": "medium", "what": "Recheck queues one hash job per piece into the uncapped process-wide queue (pool.go:38); a large torrent's recheck queues up to 2^21 closures ahead of other torrents' piece verification."},
         {"where": "X/internal/bt/session/upload.go:34", "severity": "medium", "what": "picker.Have() clones the full bitfield (picker.go:603) on every incoming request, and also in updateInterest and connected: up to 256 KiB allocated per 16 KiB block served."},
         {"where": "X/internal/bt/session/upload.go:12", "severity": "medium", "what": "No internal/bt/choke package (§4.4); choking, optimistic slot and a 1 s halving rate meter are mixed into session actor code, which is hard to test and extend."},
         {"where": "X/internal/bt/session/checking.go:87", "severity": "low", "what": "Any fingerprint mismatch resets the picker and rehashes the whole torrent instead of only the suspect files; a slow peer's request is also never timed out again after the first timeout (peers.go:426)."}
       ]},
 "Y": {"correctness": 7, "architecture": 8, "design": 8, "readability": 7, "tests": 8, "performance": 8, "security": 8,
       "defects": [
         {"where": "Y/internal/bt/session/torrent.go:583", "severity": "medium", "what": "checked() overwrites t.store even for stale results; Pause during the first check, then Resume, opens a second Storage (torrent.go:398, 550), so one Storage and its file handles leak."},
         {"where": "Y/internal/bt/session/peers.go:224", "severity": "medium", "what": "Disk writes are issued from the peer read goroutine and OnPieceSent updates torrent counters from the write goroutine (peers.go:171), plus an extra uploader goroutine per peer; departs from §4.5 'peer goroutines only send events'."},
         {"where": "Y/internal/bt/session/peers.go:471", "severity": "low", "what": "Choking clears p.uploads but not blocks already handed to the uploader (readCh), so up to UploadReadAhead pieces are still sent after a choke."},
         {"where": "Y/internal/bt/session/session.go:283", "severity": "low", "what": "SaveBan uses context.Background() with no timeout inside a wg-tracked goroutine, so a stuck DB can delay Close; Add also holds s.mu across a DB write (session.go:355)."},
         {"where": "Y/internal/store/torrents.go:203", "severity": "low", "what": "Every save deletes all trackers for the torrent, including user-added ones; the hideCheck hack (torrent.go:67) reports a checking torrent as paused to satisfy a test."}
       ]},
 "prefer": "Y",
 "why": "Y's package boundaries (choke, storage-owned shared pools, a pure resume planner, a store contract) match §4.4/§4.5 and give later milestones clean seams, and its tests are behavioural and use leak checks. X is solid but keeps choking, rate tracking and request timeouts inside the session actor, with no limit on incoming handshakes or queued pool work, and copies the bitfield on hot paths. Y's defects are local and easy to fix; X's are structural."}
```
