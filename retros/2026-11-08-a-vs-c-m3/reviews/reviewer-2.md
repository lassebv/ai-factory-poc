I read the tracker, peer, piece, storage and session packages, the torrent API routes and `main.go` for both codebases. Both build and pass `go vet`, and both pass `go test -race` for `bt/...`, `api` and `cmd` (Y's `cmd` has no tests). Coverage is high in both: X is 92.8–100% across the reviewed packages, Y is 89.9–99.4%. Note that `spec/spec-audit.md` (the D-table) isn't in the tree, so I checked D1/D7/D9/D13 against the values the spec text and code comments give.

## X

**Correctness and robustness.** Concurrency is handled carefully.
- Peer `Conn` uses a mutex-guarded send queue, so `Send` never blocks the actor. A queue overflow closes the peer (`peer/conn.go:90-104`).
- Idle and keep-alive use injected-clock timers (`peer/conn.go:225-249`).
- Shutdown is ordered: cancel, close peers, wait on `Done()` for each, `wg.Wait()` for dials and jobs, then close storage (`session/torrent.go:349-374`).
- A handshaken conn that can't be delivered is closed (`session/peers.go:118-120`).
- Storage `Close` waits for in-flight calls through `active` and a `cond` (`storage/storage.go:389-407`). Rename and handle drop happen under the same lock as `acquire`, so there's no `.part` resurrection.

The weak spot is hostile peers. `picker.Received` accepts a block nobody asked for: it activates the piece and "unowns" another peer's outstanding request (`piece/picker.go:274-298`, called from `session/peers.go:229-230`). A peer can push garbage into any piece and cause repeated hash failures. `fail()` leaves the tracker loop announcing while in `error` (`session/torrent.go:266`). `Stop` sends `stopped` even if `started` never succeeded (`tracker/loop.go:65`).

**Spec and architecture.**
- §4.4 layout is followed. Each package has an explicit interface in `api_types.go`.
- The actor plus event channel is clean, and Get/List read an atomic snapshot (`session/torrent.go:39,377-407`).
- Context is passed through `Add` and `Close`.
- There are no direct `time.*` calls in `bt/`.
- §4.5 pools: these are per-`Storage` semaphores (`storage/storage.go:50-51,240,310`) plus one goroutine per written block (`session/peers.go:239`). They aren't session-wide worker pools, so N torrents get 4N disk slots.
- D1 depth: `piece/picker.go:39`. D13 peer ID: `-TD0100-`.
- Message limit is off by 4: `length` excludes the 4-byte prefix but is compared to `16<<10+13` (`peer/wire.go:132`, `peer/api_types.go:50`).
- G16 (bind failure isn't fatal, reported via `ListenErr`) is done correctly (`session/session.go:80-90`).

**Design.** Cohesion is strong. The picker tracks per-peer outstanding blocks and block owners (`piece/picker.go:78-90`), which suits endgame, choking and banning. The tracker `Loop` and `Announcer` are separate from the session. Storage, `Conn` and `Picker` are interfaces, so they're easy to fake.

**Readability.** Doc comments are thorough and naming is consistent, but there's more code (11.3k lines including tests).

**Tests.** Very good. There's a goroutine-leak check after `Close` (`session/api_types_test.go:319-424`), and fake-clock idle and keep-alive tests (`peer/api_types_test.go:472-505`). Actor-level tests cover protocol violations, hash failure and rejected blocks. Storage tests cover a blocked `acquire`, stale handles and the handle limit. There are some `time.Sleep` polls.

**Performance.**
- `Pick` builds and sorts every candidate piece on each call, and `fillAll` runs on every received block (`piece/picker.go:190-201`, `session/peers.go:243`).
- `updateInterest` clones the whole bitfield on every HAVE (`session/peers.go:210`).
- A new `Status` is allocated per event.
- Every block is its own goroutine and `WriteAt`, and verification re-reads the piece from disk.
- `os.OpenFile` runs under the storage-wide mutex (`storage/handles.go:44`).

**Security.** `filepath.IsLocal` path checks (`storage/storage.go:75-80`), a 4 MiB tracker body cap, per-torrent bitfield length cap, send-queue cap and 128-handle LRU that blocks instead of overshooting. The tracker peer queue isn't capped (`session/peers.go:41-57`).

## Y

**Correctness and robustness.**
- Blocks are accepted only if they were requested from that peer (`session/peers.go:243`).
- A second bitfield, or a HAVE out of range, drops the peer.
- Interest is toggled both ways, and the endgame sends cancels (`session/peers.go:260-266`).
- `stopped` is sent only after a successful announce.

Lifecycle hygiene is weaker:
- The dial goroutine drops a successful conn without closing it when the torrent is stopping (`session/peers.go:110`; `send` silently returns).
- Neither dial goroutines nor peer read/write goroutines are tracked, so `Session.Close` can return before they exit (`session/session.go:214-237`).
- The tests don't check for goroutine leaks.
- Peer deadlines are OS deadlines computed from `clock.Now()` (`peer/conn.go:110,165,183`), so a fake clock not set near wall time breaks them. The tests work around this by seeding the fake with real time (`peer/peer_test.go:256`).
- `handleCache.acquire` opens with `O_CREATE` (`storage/cache.go:51`). A reader holding a stale `.part` path can recreate an empty `.part` after the rename. This is latent until M4 reads.

**Spec and architecture.**
- §4.4 layout matches (`tracker/http.go`, `tiers.go`).
- §4.5 pools are done properly: session-wide fixed worker pools, 4 disk and GOMAXPROCS/2 hash (`storage/pool.go`, `session/session.go:120-121`).
- There's no context on `Add` or `Close`.
- G16 is violated: `New` fails on a bind error (`session/session.go:110-113`) and `main` silently switches to a random port (`cmd/torrentd/main.go:93-97`).
- The message limit is correct (`peer/message.go:33`).
- D1 uses an EMA rate (`session/peers.go:48-53`).

**Design.** Smaller and more concrete: no interfaces, and the announce loop lives in the session. Hashing in memory before writing means only verified data reaches disk, which is a nice property, but the memory cost is unbounded (`session/peers.go:254-258`). The endgame is already present.

**Readability.** Compact and clear (7.5k lines), with good comments.

**Tests.** Fewer and coarser: four end-to-end session tests polled with `waitFor`, no leak checks, no `cmd` tests, and no fake-clock idle-timeout test.

**Performance.** One `WriteAt` per file segment per piece, and no disk re-read for hashing. But `Pick` rescans all pieces for every new piece (`piece/picker.go:165-179`), `connectMore` walks every candidate every second (`session/torrent.go:331`), and each `Send` marshals a fresh allocation.

**Security.**
- Paths are checked with `IsLocal` (`storage/storage.go:84`).
- Piece buffers can reach piece length (up to 256 MiB) × the number of pieces in flight.
- The candidates map is never pruned, so a hostile tracker can grow it about 700k entries per 4 MiB response.
- The handle cache may briefly exceed 128 (`storage/cache.go:48`).

## Most serious defects

**X**
1. Medium: `piece/picker.go:274` accepts unsolicited blocks and steals other peers' requests, so a hostile peer can poison pieces.
2. Medium: `session/peers.go:239` plus `storage/storage.go:50` use per-torrent semaphores with one goroutine per block, not the session-wide bounded pools §4.5 asks for.
3. Medium: `piece/picker.go:190` does an O(n log n) sort per `Pick`, run on every block for every peer, and `session/peers.go:210` clones the bitfield on every HAVE.
4. Low: `peer/wire.go:132` lets a message be 4 bytes over the 16 KiB+13 limit.
5. Low: `tracker/loop.go:65` and `session/torrent.go:266` send `stopped` without a prior `started`, and the tracker loop keeps running in `error`.

**Y**
1. High: `session/peers.go:254` keeps full-piece-size in-memory buffers per in-flight piece with no cap.
2. Medium: `session/peers.go:110` leaks the conn on shutdown, and dial and peer goroutines aren't joined by `Close`.
3. Medium: `peer/conn.go:165` mixes injected-clock time with OS deadlines, which breaks T1 with a fake clock.
4. Medium: `session/session.go:110` and `cmd/torrentd/main.go:93` make a busy peer port fatal and fall back to a random port silently, against G16.
5. Low-medium: `session/peers.go:61` never prunes candidates and they're rescanned every tick; `storage/cache.go:51` can recreate `.part` files through `O_CREATE`.

## Preference

I'd rather maintain X. Its package interfaces, per-peer block ownership in the picker and disciplined shutdown, backed by leak and fake-clock tests, fit seeding, endgame and resume work better. Its main flaws (no global pools, a slow picker, accepting unsolicited blocks) are local fixes. Y's main flaws (unbounded piece buffers, goroutines nobody waits for, deadlines tied to the clock) are spread across its I/O and lifecycle design, and its thinner tests would let regressions through.

```json
{"X": {"correctness": 7, "architecture": 7, "design": 8, "readability": 8, "tests": 9, "performance": 6, "security": 7,
       "defects": [
         {"where": "internal/bt/piece/picker.go:274", "severity": "medium", "what": "Received accepts unrequested blocks (activates the piece and unowns another peer's outstanding request); a hostile peer can inject garbage into any piece and force repeated hash failures and wasted re-downloads"},
         {"where": "internal/bt/session/peers.go:239", "severity": "medium", "what": "One goroutine per written block, gated only by per-Storage semaphores (storage/storage.go:50-51); not the session-wide bounded disk/hash worker pools of §4.5, so total concurrency scales with the number of torrents"},
         {"where": "internal/bt/piece/picker.go:190", "severity": "medium", "what": "Pick collects and sorts every needed piece on each call, and fillAll runs on every received block (session/peers.go:243); updateInterest clones the full bitfield on every HAVE (session/peers.go:210): O(peers·n log n) CPU per 16 KiB block on large torrents"},
         {"where": "internal/bt/peer/wire.go:132", "severity": "low", "what": "Length excluding the 4-byte prefix is compared to 16 KiB+13, so messages up to 16 KiB+17 bytes on the wire are accepted (spec: reject over 16 KiB+13)"},
         {"where": "internal/bt/tracker/loop.go:65", "severity": "low", "what": "Stopped announce is sent even if started never succeeded; and torrent.fail (session/torrent.go:266) leaves the tracker loop announcing while the torrent is in error"}
       ]},
 "Y": {"correctness": 6, "architecture": 7, "design": 6, "readability": 8, "tests": 6, "performance": 6, "security": 5,
       "defects": [
         {"where": "internal/bt/session/peers.go:254", "severity": "high", "what": "Every in-progress piece is buffered in memory at full piece size (up to 256 MiB), one per peer working a different piece, with no cap: multi-GB memory with large-piece torrents or many peers"},
         {"where": "internal/bt/session/peers.go:110", "severity": "medium", "what": "If the torrent is stopping, t.send drops the established conn without closing it (fd leak); dial and peer read/write goroutines are not tracked, so Session.Close (session.go:214) returns before they exit, and no leak test exists"},
         {"where": "internal/bt/peer/conn.go:165", "severity": "medium", "what": "OS read/write/handshake deadlines are computed from the injected clock's Now(); with a fake clock not near wall time they fire immediately or never (T1 violated in spirit; tests seed the fake with real time to work around it)"},
         {"where": "internal/bt/session/session.go:110", "severity": "medium", "what": "Peer-port bind failure is fatal in New; main (cmd/torrentd/main.go:93-97) silently retries on a random port instead of keeping the setting and reporting the error (G16)"},
         {"where": "internal/bt/session/peers.go:61", "severity": "low", "what": "Tracker candidates are never pruned and connectMore scans the whole map every second; a hostile tracker (4 MiB compact response is about 700k peers per announce) grows memory and CPU without bound. Also storage/cache.go:51 opens with O_CREATE, so a stale-path reader can recreate an empty .part after rename"}
       ]},
 "prefer": "X",
 "why": "X has cleaner package interfaces, per-peer block ownership, disciplined shutdown and much stronger tests (goroutine-leak and fake-clock checks); its defects are local fixes, while Y's unbounded piece buffers, goroutines Close never waits for and clock-tied deadlines are spread across its I/O and lifecycle design."}
```
