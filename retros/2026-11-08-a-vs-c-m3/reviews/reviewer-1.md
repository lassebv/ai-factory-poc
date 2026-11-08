I recommend **X**, but only narrowly. Both codebases build, pass `go vet`, and pass `go test -race` five times in a row on `session`, `peer` and `tracker`. Neither uses `time.Now`/`After`/`Tick`/`NewTimer`/`NewTicker` directly in `internal/bt`, so T1 is clean in both. The real differences are in how each handles hostile or stalled peers, resource bounds, and testing.

## X

**Correctness and robustness (7).**
- The torrent is a proper actor. Events are filtered for peers that have already left (`session/torrent.go:294`). The ticker, announcer and worker pools are all wired through `ctx`.
- Shutdown order is correct: actors stop, then the pools drain, then storage closes (`session/session.go:223-236`).
- A block is accepted only if this peer has an outstanding request for it (`session/peers.go:243`). Unrequested data and endgame duplicates are counted as wasted.
- Endgame is already implemented (`piece/picker.go:181`), so a stalled peer can't hold the last blocks forever.
- Weak points:
  - Each in-progress piece gets a buffer of the full piece size (`peers.go:254-258`). Pieces can be up to 256 MiB.
  - The candidate map is never pruned, and it is scanned on every 1 s tick (`peers.go:56-90`, `torrent.go:331`).
  - Dial goroutines and the peer read/write goroutines are not waited for in `Close` (`peers.go:88`, `peer/conn.go:140`).

**Architecture (7).**
- Matches the §4.4 layout.
- Disk and hash pools are process-wide and sized per D9 (`storage/pool.go:9-13`).
- The half-open limit is a global semaphore (`session.go:122`), and the peer ID is `-TD0100-` (D13).
- Pipeline depth is `clamp(rate×2s/16KiB, 5, 250)` (`peers.go:52`).
- Gaps:
  - The pool *queue* has no bound (`pool.go:15`).
  - The 128-handle limit is per torrent, not per process.
  - There is no in-repo goroutine-leak helper, which §4.5 requires.

**Design (7).** Concrete types with tight cohesion. `Picker` is a clean, actor-owned piece of logic. Moving to rarest-first, endgame and choking later is straightforward, but there are few interface seams for tests.

**Readability (8).** Small files, clear names, and good comments explaining *why*.

**Tests (6).** 28 tests, which reach high coverage. Many are broad scenario tests that rely on `waitFor` polling. There are no leak checks and fewer hostile-input cases.

**Performance (7).**
- Writes happen once per piece per file segment, and hashing runs on the in-memory buffer (few syscalls).
- But the picker's step 1 does an `active[i]` map lookup for every piece on each block (`picker.go:159`).
- Memory per active piece is high.

**Security (6).**
- Good bounds: a 4 MiB cap on tracker responses, exact bitfield length, a 16 KiB+9 message limit, and `filepath.IsLocal` on file paths.
- But the candidate map is unbounded, and buffering whole pieces leaves memory exposed to peers that open many pieces.

## Y

**Correctness and robustness (6).**
- Shutdown is very careful. It waits for every dial, storage job and peer's `Done()` (`session/torrent.go:349-374`). `Storage.Close` waits for active calls (`storage/storage.go:389-399`). A test checks for leaked goroutines.
- Writes and verification are sequenced correctly through the `writes`/`toVerify` maps.
- Two serious gaps:
  - **The picker accepts blocks nobody requested** (`piece/picker.go:274`, `session/peers.go:221-243`), even while the peer is choking us. Any peer can inject data for any needed block.
  - **There is no endgame and no request timeout.** A peer that stays alive with keep-alives but never delivers keeps its blocks forever.

**Architecture (6).**
- Same layout as X, with the injected clock driving the idle and keep-alive timers inside `conn` (good for T1).
- But there is no worker pool. Each block write is its own goroutine, waiting on a per-torrent semaphore (`peers.go:239`, `storage.go:98-99`).
- The half-open (20) and max-peer (50) limits are per-torrent constants (`torrent.go:22-27`).

**Design (8).** Interfaces for `Session`, `Picker`, `Storage`, `Conn` and `Loop`, each with a written contract. The tracker `Loop` with `Completed` and `Stop` is a good fit for later milestones.

**Readability (7).** Thorough documentation, but leftover scaffolding comments such as "replacing stub.go" in the `api_types.go` files. The spread across more files makes the code a little harder to follow.

**Tests (8).** About 100 tests: fake clock, garbage-never-panics checks, handle-limit blocking, `Close` waiting for blocked callers, rules for completed-before-started, and a goroutine count check after `Close`.

**Performance (6).**
- One `pwrite` per 16 KiB block, then the whole piece is read back from disk to hash it (`storage.go:313-325`).
- `fillAll` runs after every block, and `Pick` allocates and sorts every needed piece whenever it starts a new one (`picker.go:190-201`).
- The download rate is a lifetime average (`peers.go:28-34`), so pipeline depth reacts slowly.

**Security (6).**
- Memory is better bounded, since nothing buffers whole pieces.
- But unsolicited data injection, no floor on the tracker interval, an `InterfaceAddrs` syscall per self-check candidate, and existing final files reused without being truncated.

## 5 most serious defects

**X**
1. `internal/bt/session/peers.go:254`: full-piece buffers are allocated on the first block and kept until the piece completes, so memory has no practical bound with large pieces.
2. `internal/bt/session/peers.go:61`: the candidate map grows without limit and is never pruned; `connectMore` walks all of it every tick.
3. `internal/bt/storage/pool.go:58`: the pool queue has no bound, and each queued job holds a whole piece buffer.
4. `internal/bt/session/session.go:214`: `Close` doesn't wait for dial or peer goroutines, and there's no leak test (§4.5).
5. `internal/bt/storage/storage.go:231` with `cache.go:51`: the path is resolved outside the lock and then opened with `O_CREATE`. Once seeding reads arrive in M4, a race with the `.part` rename can recreate an empty `.part` file.

**Y**
1. `internal/bt/piece/picker.go:274`: unrequested blocks are accepted, even while choked, so a hostile peer can poison pieces over and over and livelock the download.
2. `internal/bt/piece/picker.go:171`: no endgame and no request timeout, so a stalled but live peer can hang the download near 100%.
3. `internal/bt/session/peers.go:239`: one goroutine per block on per-torrent semaphores instead of a bounded process-wide pool; the half-open limit is also per torrent.
4. `internal/bt/piece/picker.go:190`: an O(P log P) allocate-and-sort on the actor goroutine, plus `fillAll` after every block.
5. `internal/bt/storage/storage.go:139`: an existing final-named file is written in place and never truncated, so a completed file can keep stale trailing bytes or clobber a user's file.

## Which I'd maintain

I would rather maintain X. Its hot path is already sound against hostile and stalled peers: it only accepts requested blocks, has endgame, uses global pools and limits, and writes once per piece. Y's main weaknesses sit in the picker contract and its I/O model, which would need reworking. Y does have better test coverage and cleaner interfaces, and I would port its leak checks and fake-clock tests into X.

```json
{"X": {"correctness": 7, "architecture": 7, "design": 7, "readability": 8, "tests": 6, "performance": 7, "security": 6,
       "defects": [
         {"where": "internal/bt/session/peers.go:254", "severity": "medium", "what": "Full piece-size buffer allocated per in-progress piece and held until completion; with up to 256 MiB pieces and many active pieces, memory is effectively unbounded"},
         {"where": "internal/bt/session/peers.go:61", "severity": "medium", "what": "Tracker candidate map grows without bound and is never pruned; connectMore iterates it on every 1 s tick (torrent.go:331)"},
         {"where": "internal/bt/storage/pool.go:58", "severity": "medium", "what": "Worker pool queue is unbounded; queued hash/disk jobs each hold a whole piece buffer"},
         {"where": "internal/bt/session/session.go:214", "severity": "medium", "what": "Close does not wait for dial goroutines (peers.go:88) or peer read/write goroutines (conn.go:140); no in-repo goroutine-leak test as §4.5 requires"},
         {"where": "internal/bt/storage/storage.go:231", "severity": "low", "what": "Path resolved outside the lock, then acquired with O_CREATE (cache.go:51); once M4 reads arrive, a race with the .part rename can recreate an empty .part"}
       ]},
 "Y": {"correctness": 6, "architecture": 6, "design": 8, "readability": 7, "tests": 8, "performance": 6, "security": 6,
       "defects": [
         {"where": "internal/bt/piece/picker.go:274", "severity": "high", "what": "Received accepts blocks never requested from that peer, even while choked (session/peers.go:221); a hostile peer can inject data for every needed block and livelock the download by poisoning pieces after each PieceFailed"},
         {"where": "internal/bt/piece/picker.go:171", "severity": "medium", "what": "No endgame and no request timeout: blocks owned by a stalled peer that stays alive with keep-alives are never re-requested, so the download hangs"},
         {"where": "internal/bt/session/peers.go:239", "severity": "medium", "what": "One goroutine per block on per-torrent semaphores (storage.go:98) instead of a bounded process-wide pool; half-open limit is also per torrent (torrent.go:24)"},
         {"where": "internal/bt/piece/picker.go:190", "severity": "medium", "what": "Pick allocates and sorts all needed pieces when starting a new piece, and fillAll runs after every block (peers.go:243): O(peers·P log P) CPU on the actor"},
         {"where": "internal/bt/storage/storage.go:139", "severity": "low", "what": "Existing final-named file is used in place and never truncated, so completed files can keep stale trailing bytes or overwrite a user's file"}
       ]},
 "prefer": "X",
 "why": "X's hot path already handles hostile and stalled peers correctly (only requested blocks accepted, endgame, global pools and half-open limit, one write per piece). Y's main defects are in the picker contract and I/O model and need rework, while its stronger tests and interfaces could be ported into X."}
```
