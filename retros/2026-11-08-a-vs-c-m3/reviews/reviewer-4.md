I'd rather maintain X. It is more robust and better structured, and its tests are stronger. Y is smaller and has some good ideas, but it has a real memory-bound problem and looser shutdown handling. Both build, pass `go vet`, and pass `go test -race`, including five repeated runs of the session, peer and tracker packages. Neither calls `time.Now` or `time.After*`/`NewTimer`/`NewTicker` directly in `internal/bt`.

## X

**Correctness and robustness.** Shutdown is handled carefully:
- Every dial and storage job runs in `t.wg` (`session/torrent.go:180`).
- `shutdown` closes all peers, waits on each `conn.Done()`, waits on `t.wg`, then closes storage (`torrent.go:349-374`).
- `Session.Close` honours a context (`session.go:212-216`).
- In the peer conn, a separate supervisor goroutine runs the idle and keep-alive timers on the injected clock, so a peer that neither reads nor writes still times out (`peer/conn.go:225-249`). The handshake is cancelled through `context.AfterFunc` and leaves no stray deadline behind (`peer/wire.go:60-76`).
- Message length and ID are checked before any allocation (`wire.go:110-149`).
- Storage counts users with `enter`/`leave`, and `Close` waits for calls in progress (`storage/storage.go:161,389`).

Weak spots:
- The picker accepts blocks this peer was never asked for (`piece/picker.go:274` ignores the peer key).
- Each block write starts its own goroutine (`session/peers.go:239`).
- Nothing times out a request, so a peer that unchokes us and then sits on our requests holds those blocks indefinitely.

**Architecture.**
- Actor model: the torrent actor owns all torrent state, peer read goroutines only send events (`peers.go:136-137`), and `Get`/`List` read an atomic snapshot (`torrent.go:376-407`).
- Clock (T1): the injected clock is used everywhere, including the tracker loop timers (`tracker/loop.go:151`).
- Tracker placement: the announcer and loop live in `tracker/`, as §4.4 says.
- Worker pools: the §4.5 pools are only semaphores inside each torrent's storage (`storage/storage.go:98-99`), so the limits are per torrent and the number of goroutines is unbounded.
- D1 pipeline depth: it uses the peer's average rate since connecting (`peers.go:28-34`), not a current rate.
- Message-size limit: 16 KiB + 13 is applied to the length prefix (`peer/api_types.go:50`), which is 4 bytes looser than a strict reading.

**Design.** Each package has a contract file with small interfaces (`Storage`, `Picker`, `Conn`, `Loop`, `Announcer`, `Session`). These give clean seams for later work on choking, endgame and UDP trackers. The picker keeps per-block owners, which suits endgame.

**Tests.** There are about 96 tests. They cover protocol violations, hash failures, a busy port, close honouring its context, and fake-clock keep-alive and idle behaviour. They check for leaked goroutines by counting `runtime.NumGoroutine` (`peer/api_types_test.go:340`, `session/api_types_test.go:319`). Some use short polling sleeps; all stayed stable over 5 repeated runs.

**Performance.**
- `Pick` scans all pieces and sorts the candidates on every call, on the actor (`picker.go:178-201`).
- `updateInterest` is O(N) for each `have` message (`peers.go:211`).
- Verifying a piece reads it back from disk, in 1 MiB chunks (`storage.go:313`).
- On the plus side, outgoing pieces are written without copying (`wire.go:197`).

**Security.** Paths are checked with `filepath.IsLocal` (`storage.go:77`). Tracker bodies are capped at 4 MiB (`tracker/client.go:23`). The bitfield is capped at the torrent's exact size, and the send queue has a limit. The tracker peer queue is unbounded (`peers.go:55`).

## Y

**Correctness and robustness.**

Strengths:
- The disk and hash pools are shared across the session (`session/session.go:120-121`), and the half-open limit is a session-wide semaphore (`session.go:122`).
- Pipeline depth uses a per-tick rolling average (EMA) of the rate, matching D1 (`peers.go:48-53`).
- Endgame mode works, and duplicate requests are cancelled (`piece/picker.go:181`, `peers.go:261-266`).
- Blocks that weren't requested are rejected (`peers.go:243`).

Weaknesses:
- Each piece being downloaded is buffered whole in memory (`peers.go:254-258`). With 256 MiB pieces this can reach gigabytes.
- Dial goroutines are not tracked. A connection is leaked if `evConnected` is not delivered, or is still queued when the actor exits (`peers.go:88,110`; `torrent.go:272`).
- `Close` does not wait for peer goroutines and takes no context (`session.go:214`).
- Blocks that arrive after a choke are thrown away and counted as wasted (`peers.go:206-211`).

**Architecture.**
- Layout: `peer/handshake.go`/`message.go`/`conn.go` match §4.4 well. The announce loop, however, lives in `session/announce.go` rather than in `tracker/`.
- Clock (T1): times from the injected clock are fed into OS socket deadlines (`peer/conn.go:110,165,183`). With a fake clock these deadlines mean nothing; the tests only work because the fake clock starts at real time (`peer_test.go:256`).
- Busy peer port (G16): Y silently switches to a random port (`cmd/torrentd/main.go:93-98`) instead of reporting the error. It also ignores the `listen_port` setting (`main.go:89-92`).

**Design.** It uses concrete types, with an `Engine` interface on the API side. It is simple, but there are fewer seams for later milestones. The storage API works on whole pieces only (`storage.go:189`), which will need changing for seeding reads by block.

**Tests.** There are 28 tests. They are mostly good integration scenarios: a corrupted piece, bad peers, tracker retry. There are no goroutine-leak checks and fewer edge-case unit tests. They are deterministic.

**Performance.**
- Good: one write per piece and hashing in memory, with no read-back.
- Each sent message is allocated, and piece payloads are copied (`message.go:63-87`).
- A read deadline is set for every message (`conn.go:165`).
- `Pick` allocates a map per call and rescans O(N) for each new piece (`picker.go:137,165-172`).
- The pool queue is unbounded (`pool.go:58`).

**Security.** Paths are checked with `IsLocal` (`storage.go:84`), and tracker bodies are capped at 4 MiB. Memory is not bounded (piece buffers plus the unbounded pool queue). The handle cache can go over its 128 limit when every handle is busy (`cache.go:46-49`), and it opens files with `O_CREATE` (`cache.go:51`). Preallocation can shrink an existing file (`storage.go:123`).

```json
{"X": {"correctness": 8, "architecture": 7, "design": 8, "readability": 8, "tests": 8, "performance": 6, "security": 7,
       "defects": [
         {"where": "X/internal/bt/session/peers.go:239", "severity": "medium", "what": "Each block write starts a new goroutine. The disk and hash 'pools' are only semaphores inside each torrent's storage (storage/storage.go:98-99), so §4.5's bounded pools hold per torrent only. Goroutines and buffered 16 KiB blocks pile up without limit when the disk is slower than the network."},
         {"where": "X/internal/bt/piece/picker.go:274", "severity": "medium", "what": "Received ignores the peer key, so any peer's unrequested block for a needed slot is accepted and written. A hostile peer can inject data into pieces it was never asked for (caught only by the hash check, which wastes the whole piece) and take blocks away from honest peers."},
         {"where": "X/internal/bt/piece/picker.go:178", "severity": "medium", "what": "Pick scans every piece and sorts all candidates on each call, on the actor goroutine, and it is called after every received block. For torrents with tens of thousands of pieces this caps throughput. updateInterest (session/peers.go:211) is also O(N) for each have message."},
         {"where": "X/internal/bt/session/peers.go:254", "severity": "medium", "what": "No request timeout or snub detection, and pipeline depth uses the average rate since connecting (peers.go:28). A peer that unchokes us, then sends only keep-alives, holds its outstanding blocks forever and can stall the end of the download."},
         {"where": "X/internal/bt/session/torrent.go:349", "severity": "low", "what": "shutdown does not drain t.events. An evHandshaken already queued when the actor quits holds a net.Conn that is never closed (dial only closes the connection if send fails, peers.go:118). There is a similar race for evOpened."}
       ]},
 "Y": {"correctness": 6, "architecture": 6, "design": 6, "readability": 7, "tests": 6, "performance": 6, "security": 5,
       "defects": [
         {"where": "Y/internal/bt/session/peers.go:256", "severity": "high", "what": "Each piece being downloaded is buffered whole in memory (make([]byte, PieceSize)), with no limit on how many pieces are buffered. Buffers for partly downloaded pieces stay after peers disconnect, and completed buffers wait in an unbounded pool queue (storage/pool.go:58). With 256 MiB pieces and dozens of peers this reaches gigabytes, far beyond the RSS goals."},
         {"where": "Y/internal/bt/session/peers.go:110", "severity": "medium", "what": "Dial goroutines are not tracked by any WaitGroup. If t.send(evConnected) is dropped because the context was cancelled, or the event is still in the 1024-slot buffer when the actor exits, the established connection is never closed. Session.Close (session.go:214) also does not wait for peer read/write goroutines and has no context or timeout."},
         {"where": "Y/cmd/torrentd/main.go:93", "severity": "medium", "what": "G16 violation: when the peer port is busy, Session.New fails and main silently rebinds to a random port instead of keeping the UI up and reporting the error. An explicit --peer-port is overridden, and the listen_port setting is never read (main.go:89-92)."},
         {"where": "Y/internal/bt/peer/conn.go:165", "severity": "low", "what": "Read, write and handshake deadlines are computed from the injected clock but enforced by the OS against wall time. With a fake clock that has been advanced or does not start at real time, connections time out at once or never, which breaks T1 determinism. It also sets a deadline for every message."},
         {"where": "Y/internal/bt/storage/cache.go:51", "severity": "low", "what": "The handle cache opens files with O_CREATE, and Storage.io reads the path outside the cache lock (storage.go:231-237). A read racing the .part-to-final rename (seeding in M4) recreates an empty .part file. The cache can also exceed its 128-handle limit when all handles are busy (cache.go:48), and preallocate shrinks an existing larger file (storage.go:123)."}
       ]},
 "prefer": "X",
 "why": "X has tighter goroutine-lifecycle and shutdown discipline, uses the injected clock correctly in the peer layer, and has small contract interfaces that make choking, endgame and resume work easy to slot in. Its main weaknesses (per-block goroutines, per-torrent semaphores instead of shared pools, an O(N log N) picker) are local fixes. Y's whole-piece in-memory buffering, untracked dials and port fallback that breaks G16 are harder to fix, and its thinner tests give less protection when changing it."}
```
