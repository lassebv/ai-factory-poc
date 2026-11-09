Both trees build, pass `go vet` and pass `go test -race -count=1 ./...` with no failures. I didn't modify any files. The findings below come from reading the M4 code paths. I found the races and stalls by reading the code and did not reproduce them with tests.

## X

**Correctness and robustness.** The actor model is careful. Lifecycle actions go through their own unbuffered `actions` channel and take a `ctx` (`session.go:385-410`), so a command can't get stuck behind 1024 queued events. Shutdown keeps handling events while peers and jobs finish, so a read goroutine waiting for a block decision can't deadlock it (`torrent.go:690-725`, `drainEvent`).

Checking runs under a context that pause cancels (`deactivate`, `torrent.go:400-405`; `check`, `torrent.go:560`). Request expiry lives in the picker and uses the injected clock (`piece/picker.go:382-419`). It includes a per-peer cooldown and frees exclusive pieces, so a peer that stalls cannot keep blocks forever. There is a test for this: `TestContractSessionStalledPeerDoesNotHang`.

The data path gives natural back-pressure. Each received block waits for the actor to accept it, then `WriteBlock` runs on the read goroutine through the shared disk pool (`peers.go:205-226`). The fsync-before-bitfield order holds: `markDirty` runs after the write completes (`storage.go` `WriteBlock`), and flush syncs before it saves (`persist.go:62-70`).

The main real bug: if you pause during the first check and resume before the stale check job returns, storage is opened twice. `startCheck` reads `st := t.store`, which is still nil, so a second `storage.Open` happens. `checked` then assigns `t.store = ev.st` before the generation check (`torrent.go:584`). The last result to arrive wins, and the other storage is never closed. If the stale result arrives last, peers write into one `Storage` while flush and Close sync another. That breaks the C3 fsync guarantee and leaks file handles.

Other problems:
- `Session.close` returns early on timeout. It never flushes and never closes the pools or the storages (`session.go:452-456`).
- `main.go:157` writes `clean_shutdown` regardless of that failure.
- The `hideCheck` hack makes `Status` report a state the torrent is not in, just to satisfy a test (`torrent.go:70`, `publish`).

**Architecture.** It follows the §4.4 layout, including `choke/`. The process-wide disk and hash pools sit behind `storage.Pools` (`storage/pool.go`), and every storage operation goes through them. The clock is injected into the picker, the choker and the meters. `context` is passed to lifecycle actions and to handshakes (`peer/wire.go` uses `AfterFunc` for the deadline).

There are two small departures from §4.5:
- Each peer gets a third "uploader" goroutine (`peers.go:197`).
- The write goroutine changes `p.up` and `p.inflight` directly in `OnPieceSent` (`peers.go:171-180`). Both are thread-safe, but this goes against "peer goroutines only send events".

**Design.** The pieces are cohesive:
- The choker is a pure function: peers and a time in, an unchoke set out (`choke/choke.go`).
- The picker owns contributor IPs (`AddPeer(key, origin)`, and `PieceFailed` returns the IPs), expiry and cooldown.
- Persistence is a snapshot taken on the actor and synced by the flusher (`persist.go`).

All of this fits M5/M6 well (magnets can add a peer source, and the queueing and limits fields are already in the record).

**Tests.** X has an in-repo goroutine-leak checker (`btest.CheckGoroutines`), used in the session tests as §4.5 requires. There are focused choke contract tests (slots, rate, anti-snub, fill, meter), picker edge cases (expiry and cooldown, released exclusives, verified while outstanding), real swarm tests with corrupt, stalling and IPv6 peers, and a stall test. Some assertions are soft: the ban test only checks bans if `WastedTotal > 0`. The integration tests use real time with 30 s waits.

**Performance.** Pick uses bucketed index sets and doesn't scan every piece. The have-bitfield is cached (`pieces.go:28-40`), and `quietEvent` skips publishing on every block. The cost is one actor round trip per received block. A recheck is serial within one torrent, so it uses one hash worker per torrent.

**Security.** Incoming handshakes are capped (`maxIncomingHandshakes`, `session.go:28,207`). A request over 16 KiB drops the peer, and the upload queue is bounded at `UploadReadAhead` / `MaxUploadQueue`. Bitfield length is capped, and banned IPs are filtered at dial and at accept.

## Y

**Correctness and robustness.** The actor itself is clean: one `events` channel, and stale results are dropped with a `gen` counter. The upload path is bounded (`maxUpReads` and `maxUpQueue`, `upload.go`), and pending writes are capped at 512 (`maxPendingWrites`). Several real defects sit at the edges.

1. **Pause doesn't stop a recheck.** `startCheck` queues one hash job per piece. Each job only checks `t.ctx`, which is cancelled only at shutdown (`checking.go:119-127`). Pause bumps `gen`, but every queued piece still gets hashed. Each pause/resume cycle queues the whole torrent again behind the old jobs, all on the shared hash pool.
2. **A slow peer can hold a block forever.** Once a peer is marked `slow`, `checkTimeouts` never releases its outstanding request again (`peers.go:426`, the `!p.slow` condition). Snubbing only changes choking. Outside endgame, that block is effectively stuck on an unresponsive peer.
3. **Race in the dirty flag.** `storage.io` sets `dirty = true` before `WriteAt` (`storage/storage.go:287`). If a concurrent `Sync` clears the flag and fsyncs between the two, the write lands after the fsync with the flag already cleared. A later flush can then save a bitfield whose data was never fsynced, which violates C3.
4. **Same Close-timeout problem as X.** On timeout, `close` returns before closing the pools and before saving the final records (`session.go:462`), and `main.go:158` still writes `clean_shutdown`.

**Architecture.** There is no `internal/bt/choke` package, so the §4.4 layout is not followed: choking is mixed into `session/upload.go`. There are process-wide pools (`session/pool.go`), but storage also creates its own per-storage semaphores (`storage.go` `disk` and `hash` channels), so concurrency limits exist in two places. The pool queue has no limit, and checking a 2²¹-piece torrent queues about 2M closures in one go.

`SetDeadline(s.clk.Now()...)` (`incoming.go:71`) passes injected-clock time to a real network deadline, which misuses T1. Lifecycle commands don't take a `ctx`, and `checkingProgress` is always 0 (`api/torrents.go:216`).

**Design.** Contributor tracking, rates, snubbing and choking are all spread across `peerState` and `torrent` fields in the session package. That makes the session the place every change has to touch. The "stale request" sequence heuristic (`peers.go` `sentReq`) is clever but hard to reason about.

**Tests.** The actor-level tests poke internals directly. `TestChokerSlotsAndRotation` uses `t.Log` where it should assert, and ends with calls that are only there for coverage (`actor_test.go:150-169`). The session has no goroutine-leak checks. The swarm tests (`swarm_test.go`) cover seed, leech, resume and pause well.

**Performance.**
- `t.picker.Have()` clones the bitfield (up to 256 KiB) on every incoming request (`upload.go:34`). It does the same on every `have` message while we're not interested (`peers.go:279`).
- `Pick` clones and sorts the active list, then scans every piece, on each refill.
- `isSelf` calls `net.InterfaceAddrs` for each candidate on our port. X has the same code.

**Security.** Incoming handshake goroutines are unlimited (`incoming.go:41`). The other limits (message size, bitfield, request length, upload queue) are fine. The `strikes` map is only pruned when a ban expires.

```json
{"X": {"correctness": 7, "architecture": 8, "design": 8, "readability": 7, "tests": 7, "performance": 8, "security": 8,
       "defects": [
         {"where": "X/internal/bt/session/torrent.go:584", "severity": "high", "what": "Pause then resume during the first check opens a second Storage (startCheck reads st=nil), and checked() assigns t.store before the gen check. One storage leaks; if the stale result arrives last, peers write to one Storage while flush and Close sync the other, which breaks C3."},
         {"where": "X/cmd/torrentd/main.go:157", "severity": "medium", "what": "clean_shutdown is written even when Session.Close timed out and skipped the final flush, so the next start uses the clean (mtime-trusting) fast-resume path on stale records."},
         {"where": "X/internal/bt/session/session.go:455", "severity": "medium", "what": "Close returns early on ctx timeout without flushing or closing storages and pools, leaking workers and file handles."},
         {"where": "X/internal/bt/session/torrent.go:70", "severity": "low", "what": "The hideCheck hack makes Status report 'paused' while the torrent is checking, to satisfy a test; observed state and real state disagree."},
         {"where": "X/internal/bt/session/peers.go:171", "severity": "low", "what": "The peer write goroutine changes peerState (p.up, p.inflight) directly and each peer has an extra uploader goroutine, against the §4.5 rule that peer goroutines only send events; recheck is serial within one torrent (torrent.go:560)."}
       ]},
 "Y": {"correctness": 5, "architecture": 5, "design": 6, "readability": 7, "tests": 5, "performance": 5, "security": 6,
       "defects": [
         {"where": "Y/internal/bt/session/checking.go:119", "severity": "high", "what": "Pause does not cancel a check: queued per-piece hash jobs only check t.ctx, so the whole torrent is still hashed, each pause/resume queues it again, and the unbounded queue can hold millions of closures on the shared hash pool."},
         {"where": "Y/internal/bt/storage/storage.go:287", "severity": "medium", "what": "dirty is set before WriteAt; a concurrent Sync can clear it and fsync before the write lands, so a later flush saves a bitfield whose data was never fsynced (C3)."},
         {"where": "Y/internal/bt/session/peers.go:426", "severity": "medium", "what": "After a peer is marked slow its outstanding request is never released again (snub only affects choking), so an unresponsive peer holds a block until endgame."},
         {"where": "Y/internal/bt/session/session.go:462", "severity": "medium", "what": "Close returns on timeout without closing pools or saving final records, and main.go:158 still writes clean_shutdown."},
         {"where": "Y/internal/bt/session/upload.go:34", "severity": "medium", "what": "picker.Have() clones the full bitfield (up to 256 KiB) on every incoming request and on every have message (peers.go:279); there is no choke/ package (§4.4); incoming handshakes are unlimited (incoming.go:41); the net deadline uses the injected clock (incoming.go:71)."}
       ]},
 "prefer": "X",
 "why": "X follows the spec structure (a pure choke package, a picker that owns expiry and contributors with the injected clock, storage-level process-wide pools, ctx on actions, bounded incoming handshakes) and has leak-checked tests. Its main bug is a narrow pause/resume race during checking. Y works but puts choking and expiry logic inside session, loses work on pause, has a fsync-ordering race and a slow-peer stall, and does allocation-heavy per-message work, so it would need more rework before M5 and M6."}
```
