# Comparative review of the milestone 4 swarm engine in X and Y

Both trees build, pass `go vet` and pass `go test -race` across `internal/...` and `cmd/...`. Coverage of the M4 packages is above 80% in both (X: session 87%, piece 98%, store 88%; Y: session 84%, piece 98%, choke 96%, store 84%). The findings below come from reading the code. None of them is shown by the existing tests.

## X

**Architecture.** X has no `internal/bt/choke` package, which §4.4 lists explicitly. Choking, optimistic unchoke and anti-snubbing live inside `session/upload.go`, mixed with upload serving. The lifecycle methods take no `context` (`session/session.go:277`), so §4.5 "context everywhere" is broken at the API boundary. The process-wide disk and hash pools sit in `session/pool.go`. `submit` never blocks and the queue is unbounded; "callers bound the work", but the checker does not. The injected clock is used consistently. The one misuse is `incoming.go:71`, which sets a real socket deadline from `s.clk.Now()`; that is wrong under a fake clock. The 16 KiB request check and disconnect (T6) are present (`upload.go:23-29`).

**Correctness.** The actor model is sound: peers and jobs only send events, `t.send` stops on `ctx.Done()`, and shutdown waits for `Done()` on every conn and for `t.wg`. Flushing captures a snapshot, then syncs, then saves, which meets C3. I found three real defects:

1. **Pause then Resume while the initial open job is in flight leaves the torrent stuck in `checking`.** `activate` skips `open()` because `t.opening` is still set (`checking.go:17`). The open result then arrives with a stale `gen` and returns before activating (`checking.go:95`). Another Resume is a no-op in `checking`, so the user must pause and resume again.
2. **A peer that stops answering can hold up to 500 blocks until endgame.** After one 30 s timeout the peer is flagged `slow` and never released again (`peers.go:426`). Because requests go stale after 3 s, `fill` keeps giving it one more block every tick, up to `maxOutstanding` = 500 blocks. Those pieces stall until endgame.
3. **A recheck floods the shared hash pool, and pausing does not cancel it.** `startCheck` queues every piece at once on the FIFO hash pool (`checking.go:114-119`). This starves piece verification for every other torrent. Stale jobs from a paused check still hash the full piece, because they only test the torrent's `ctx`.

**Design and readability.** X is cohesive and easy to read. The files are small and clearly named (`checking.go`, `resume.go`, `upload.go`, `pool.go`). Banning a peer discards the pieces it contributed to (`applyBan`), which is a thoughtful touch. The cost of keeping the choker in the session is that it cannot be reused or unit-tested by itself, and that will matter for M6 seeding limits and stats.

**Performance.** Each `Pick` scans every piece and sorts the active list (`picker.go:227-330`). `updateInterest` is O(N) per `have` message until the peer becomes interesting (`peers.go:280`). Endgame duplicates have no per-block cap (`picker.go:349`). `uploaded` is counted when a message is queued, not when it is written (`upload.go:75`).

**Security and bounds.** Incoming handshakes start one goroutine each with no cap (`incoming.go:41`); each is limited only by its 10 s deadline. Upload queues are bounded at 512 requests and 4 reads in flight per peer, and the bitfield length is capped.

**Tests.** The white-box actor tests use fake connections and a fake clock and are deterministic. They cover the choker, upload serving, timeouts, strikes and discard. There are also picker tests for M4 and loopback swarm and resume tests. Nothing covers pause or resume racing the open or check jobs.

## Y

**Architecture.** Y follows the §4.4 layout, including a standalone `choke` package with a `Meter`, `Rechoke` and `Fill`. It adds `btest`, a shared in-repo goroutine-leak helper as §4.5 asks. The bounded process-wide pools live in `storage/pool.go` and are injected through `storage.Options{Pools}`. Lifecycle actions carry the request `context` from the API through to the actor (`session.go:385-415`). The picker takes an injected clock and handles request expiry itself (`Expire`, 20 s, with a cooldown per peer).

**Correctness.** Block writes run on the peer's read goroutine, but only after a synchronous accept or reject decision from the actor (`evBlock`). This gives real TCP back-pressure. Shutdown keeps draining events until helpers exit, which avoids deadlocks. Checking runs in a cancellable goroutine with progress reporting. Defects:

1. **Storage double-open.** If a torrent is paused and resumed before its first check returns, Resume starts a second check that opens a second Storage (`torrent.go:398`). `checked()` adopts `ev.st` before it tests the generation (`torrent.go:583`), so one Storage and its file handles leak. Two handles also stay open on the same `.part` files.
2. **The uploader leaks its in-flight count on read errors.** On a read error it closes the conn but never decrements `inflight` (`peers.go:198`). This is harmless now because the conn dies, but it is fragile.
3. **`hideCheck` is a hack for a contract test.** It reports a check as `paused` (`session.go:351`), so a torrent added paused still reads its files.
4. **`SaveTorrents` deletes all tracker rows.** It runs `DELETE` on every tracker row for the torrent (`store/torrents.go:203`), which will erase user-added trackers in M5.
5. **`Close` skips the final flush on timeout.** If the context expires, `Close` returns without flushing or closing storage (`session.go:455`). X has the same issue.

**Design.** Y has clear package seams: `choke.Peer` views, a `piece.Receipt` struct, `Options`, and `storage.PlanResume` and `Stat` as pure functions. These fit M5 and M6 well. `torrent.go` is 791 lines, close to the 800-line gate. It also has a lot of state flags (`savedBF`, `fullRecheck`, `hideCheck`, `checkGen`).

**Performance.** The picker uses availability buckets, so rarest-first does not scan every piece. Endgame is capped at 3 owners per block. The cached wire bitfield is reused. `publish` is skipped for frequent events. Costs: one channel round trip to the actor per 16 KiB block, and hashing reads files directly, outside the disk pool.

**Security and bounds.** Incoming handshakes are capped by a semaphore of 64. Redials back off. The upload queue is limited to 256 and read-ahead to 4. Pipelines are capped at 250 outstanding requests.

**Tests.** The choke and picker contract tests are deterministic and test behaviour directly (expiry and cooldown, release, verified while outstanding). The session tests cover a corrupt-peer ban, a stalled peer, missing files, persistence and lifecycle actions, and they use the leak helper. A few tests use `time.Sleep`.

## Five most serious defects

**X**
1. `session/checking.go:95` (high): Pause then Resume during the open job leaves the torrent stuck in `checking`.
2. `session/peers.go:426` (medium): a `slow` peer is never timed out again and can hold up to 500 blocks until endgame.
3. `session/checking.go:119` (medium): a recheck floods the FIFO hash pool, starving other torrents, and paused checks are never cancelled.
4. `session/incoming.go:41` (medium): no cap on concurrent incoming handshakes.
5. `piece/picker.go:349` (low): endgame duplicates have no per-block cap.

**Y**
1. `session/torrent.go:583` (medium): Pause and Resume during the first check open a second Storage and leak one.
2. `session/peers.go:198` (low): the uploader does not decrement `inflight` on read errors.
3. `session/session.go:351` (low): `hideCheck` reports checking as paused, a test-driven hack.
4. `store/torrents.go:203` (low): `SaveTorrents` deletes all tracker rows, including future user-added ones.
5. `session/session.go:455` (low): `Close` on timeout skips the final flush and storage close.

## Which I'd rather maintain

I would rather maintain Y. It follows the specified package layout and concurrency model more faithfully, with a separate choker, injected pools, context on actions, a shared leak helper and back-pressured writes. Its picker and choker are separately testable components that M5 and M6 can build on. X is tidy and readable, but its defects are more serious (a stuck state, block hoarding by hostile peers, and hash-pool starvation), and its missing `choke` package is a structural gap.

```json
{"X": {"correctness": 6, "architecture": 5, "design": 6, "readability": 8, "tests": 7, "performance": 6, "security": 5,
       "defects": [
        {"where": "X/internal/bt/session/checking.go:95", "severity": "high", "what": "Pause then Resume while the open job is in flight: activate skips open() (opening=true) and the stale-gen result returns early, so the torrent is stuck in checking"},
        {"where": "X/internal/bt/session/peers.go:426", "severity": "medium", "what": "Once a peer is flagged slow it is never timed out again; stale-request logic keeps feeding it up to 500 blocks, which stall until endgame"},
        {"where": "X/internal/bt/session/checking.go:119", "severity": "medium", "what": "Recheck enqueues every piece at once on the shared FIFO hash pool, starving other torrents' verification; paused or stale checks are not cancelled and still hash"},
        {"where": "X/internal/bt/session/incoming.go:41", "severity": "medium", "what": "No cap on concurrent incoming handshake goroutines (connection-flood resource exhaustion)"},
        {"where": "X/internal/bt/piece/picker.go:349", "severity": "low", "what": "Endgame duplicates have no per-block owner cap, wasting bandwidth across many peers"}]},
 "Y": {"correctness": 7, "architecture": 8, "design": 8, "readability": 7, "tests": 8, "performance": 7, "security": 7,
       "defects": [
        {"where": "Y/internal/bt/session/torrent.go:583", "severity": "medium", "what": "Pause and Resume during the first check start a second check that opens a second Storage; checked() adopts ev.st before the gen test, so one Storage and its file handles leak"},
        {"where": "Y/internal/bt/session/peers.go:198", "severity": "low", "what": "Uploader closes the conn on a read error without decrementing inflight, a fragile read-ahead accounting invariant"},
        {"where": "Y/internal/bt/session/session.go:351", "severity": "low", "what": "hideCheck reports a running check as paused to satisfy a contract test; a paused add still reads files"},
        {"where": "Y/internal/store/torrents.go:203", "severity": "low", "what": "SaveTorrents deletes all tracker rows, which will erase user-added trackers in M5"},
        {"where": "Y/internal/bt/session/session.go:455", "severity": "low", "what": "Close returns on context timeout without the final flush or storage close, losing progress since the last flush"}]},
 "prefer": "Y",
 "why": "Y follows the spec's package layout and concurrency model more faithfully (separate choke package, injected pools, context on actions, leak helper, back-pressured writes) and has fewer and less severe bugs; X is readable but has a stuck-state bug, block hoarding by hostile peers, hash-pool starvation, and no choke package."}
```
