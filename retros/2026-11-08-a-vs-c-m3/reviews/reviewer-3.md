# Comparative review of `torrentd` Milestone 3: X vs Y

Both codebases build and are clean under `go vet`. Their tests pass under `-race`, including 8 repeated runs of `./internal/bt/...` with no flakes. Coverage of the `internal/bt/*` packages is 89.9–98% for X and 95–100% for Y. Neither uses `time.Now`, `time.After` or similar directly in `internal/bt` or `internal/store`. Neither has the in-repo goleak-style helper that §4.5 asks for. `spec/spec-audit.md` is not in the tree, so I checked D1/D7/D9/D13 against the inline comments that cite them: D1 pipeline depth, D7 numwant and key, D9 pool and handle defaults, D13 peer ID.

## X

**Correctness and robustness.** The actor in `session/torrent.go:242-281` is tidy: one goroutine, a bounded event channel, and a 1 s tick from the injected clock. Pools are process-wide (`session.go:120-122`) and so is the half-open semaphore (`session.go:122`, `peers.go:81-85`). Unrequested or cancelled blocks count as waste and are never written (`peers.go:243-246`). Endgame exists and sends cancels (`piece/picker.go:180-187`, `peers.go:261-266`), so a peer that sits on requests can't stall the end of a download.

The weak points are about resources more than logic:
- Every active piece is assembled in a heap buffer of the full piece length (`peers.go:254-258`). With 16 MiB pieces and 50 peers each starting a piece, that is hundreds of MiB or more.
- The pools have unbounded queues and no context (`storage/pool.go:58-67`), so there is no backpressure from disk to network.
- Dial goroutines and peer `Conn` goroutines aren't tracked. `Session.Close` can return while they are still unwinding (`peers.go:88`, `peer/conn.go:155-159`).

**Spec and architecture.**
- §4.4 layout is followed.
- §4.5 is mostly followed: actor model, global disk pool of 4 and hash pool of GOMAXPROCS/2 (D9), 128 handles per torrent.
- T1 is subtly broken. OS socket deadlines are computed from the injected clock (`peer/conn.go:110,165,183`), so a fake clock set in the past or future gives wrong real deadlines.
- Wire details are good: `MaxMessageLen` is exactly 16 KiB + 9 (`peer/message.go:33`), the bitfield is capped at the exact torrent size, the D1 depth uses a smoothed rate (`peers.go:48-53`), and the peer ID prefix matches D13.

**Design.** Concrete types with little abstraction, but cohesive. Whole-piece buffering and the `ReadPiece`-only storage API fit seeding (block reads) poorly. The picker is a linear scan.

**Readability.** Compact and well commented; easy to follow.

**Tests.** There are only 4 session tests, all end-to-end with real time and `waitFor` polling. There are no leak checks and few adversarial actor tests. Package tests for the picker, storage and peer layers are solid but thin.

**Performance.** There is one `WriteAt` per file segment per piece, the piece is hashed from memory (no read-back), and writes are coalesced and flushed in batches. But `publish()` runs after every event and calls `Bitfield.Full()`, an O(n) count, for every peer (`torrent.go:204-208,218`). `Pick` scans every piece once per new piece picked (`picker.go:159-179`).

**Security.** Paths are checked with `filepath.IsLocal`, tracker bodies are capped at 4 MiB, and peer messages are capped. Weak spots are the piece buffers above and a `candidates` map that grows without bound (`peers.go:61`).

## Y

**Correctness and robustness.**
- Shutdown is careful: `torrent.shutdown` (`session/torrent.go:349-374`) waits for every `Conn.Done`, every job, and the stopped announce, all bounded by a context.
- The `Conn` idle and keep-alive timers run on the injected clock in a supervisor goroutine (`peer/conn.go:225-249`), which is the correct T1 design.
- Storage uses `O_RDWR` without `O_CREATE`, stale-handle tracking, and `Close` waits for calls in flight (`storage/storage.go:389-407`).

Real bugs:
1. **Unrequested blocks are accepted.** `Picker.Received` ignores which peer sent a block, and even starts a fresh piece for it (`piece/picker.go:274-286`). Each accepted block then gets its own unbounded goroutine (`session/peers.go:239-241`). A hostile peer can push data with no requests and pile up goroutines and 16 KiB buffers faster than the 4-slot disk semaphore drains them.
2. **No endgame and no request timeout.** A peer that accepts requests and only sends keep-alives keeps its blocks outstanding forever, and the torrent stalls at 99% (`picker.go:171-209`).
3. **No minimum announce interval.** A tracker returning `interval=1` gets announced to every second (`tracker/loop.go:92-97`).

**Spec and architecture.**
- §4.4 layout is followed.
- §4.5 pools are not process-wide. They are per-`Storage` semaphores (`storage.go:98-99`), with goroutine-per-job (`torrent.go:180`), so N torrents means 4N disk slots and N×GOMAXPROCS/2 hash slots.
- Half-open is limited per torrent, not globally (`torrent.go:24`).
- D1 uses the average rate over the connection's lifetime, not a smoothed one (`peers.go:28-34`).
- `MaxMessageLen` is 16 KiB + 13 as a length prefix, 4 bytes looser than needed (`peer/api_types.go:50`).
- Using `context` everywhere and injecting the clock (T1) are done well.

**Design.** Each package exposes a documented interface contract (`Conn`, `Storage`, `Picker`, `Session`, `Loop`). The picker is keyed by `PeerKey` and tracks per-peer outstanding blocks, which suits choking and endgame later. Streaming `WriteBlock`/`ReadBlock` with chunked `VerifyPiece` (`storage.go:301-328`) fits seeding and resume.

**Readability.** Very well documented; contracts sit in the `api_types.go` files and behaviour notes are explicit.

**Tests.** Much broader. The actor is unit-tested by driving `handle()` directly with a fake clock (`session/session_test.go:109`). There are tests for protocol violations, garbage input that must never panic, the handle limit blocking until release, `Close` waiting for blocked acquires, and goroutine-count checks. Some tracker and peer tests use `time.Sleep`.

**Performance.** One goroutine and one `WriteAt` per block. Every piece is read back from disk to hash it (double I/O). `Pick` builds and sorts a candidate list of every needed piece on each call (`picker.go:190-201`). `updateInterest` clones the whole bitfield on every `have` (`picker.go:359`, `peers.go:210`).

**Security.** Paths are checked with `IsLocal`, verify buffers are bounded, the handle limit is strictly enforced (it blocks rather than overshooting). Weak spots are the unrequested-block flood, no write deadline (`peer/conn.go:190-217`), and the per-torrent half-open limit.

## Verdict

I'd rather maintain Y, by a small margin. Its packages have explicit, documented interfaces, a streaming storage design and a picker that tracks peers, all of which suit seeding, endgame and resume. Its test suite is much more deterministic and adversarial, so changes are safer to make. X is more operationally correct right now: it already has endgame, global pools and rejects unrequested blocks. Y's defects are concentrated and fixable: owner-checked `Received`, endgame or request timeouts, process-wide pools, and an interval floor. X's whole-piece buffering and O(n) hot paths would need reworking.

```json
{"X": {"correctness": 7, "architecture": 7, "design": 6, "readability": 8, "tests": 6, "performance": 6, "security": 6,
       "defects": [
         {"where": "internal/bt/session/peers.go:254", "severity": "high", "what": "Every active piece is buffered in memory at full piece length (up to 256 MiB) until it is hashed and written; memory grows with active pieces times piece length, with no bound."},
         {"where": "internal/bt/storage/pool.go:64", "severity": "medium", "what": "Disk and hash pools have unbounded queues and no context; queued jobs hold piece buffers, so a slow disk turns into unbounded memory with no backpressure to the network."},
         {"where": "internal/bt/session/torrent.go:204", "severity": "medium", "what": "publish() runs after every event and calls Bitfield.Full() (an O(n) count) for every peer plus picker.Complete(); with large bitfields this is MBs of scanning per message. Pick (piece/picker.go:159-179) is also O(numPieces) per new piece."},
         {"where": "internal/bt/session/peers.go:88", "severity": "medium", "what": "Dial goroutines and peer Conn goroutines (peer/conn.go:155) are not tracked by any WaitGroup, so Session.Close returns before they exit; there is also no goleak-style helper (required by §4.5)."},
         {"where": "internal/bt/peer/conn.go:165", "severity": "low", "what": "OS socket deadlines are computed from the injected clock (also lines 110 and 183); with a fake clock, idle and handshake timeouts fire immediately or never, which breaks T1 testability."}
       ]},
 "Y": {"correctness": 6, "architecture": 6, "design": 8, "readability": 8, "tests": 8, "performance": 6, "security": 6,
       "defects": [
         {"where": "internal/bt/piece/picker.go:274", "severity": "high", "what": "Received ignores which peer sent a block and accepts unrequested blocks (it even starts new pieces); each accepted block spawns an untracked-size goroutine (session/peers.go:239), so a hostile peer can flood goroutines and memory."},
         {"where": "internal/bt/piece/picker.go:171", "severity": "high", "what": "No endgame and no request timeout: a peer that holds outstanding requests and sends only keep-alives keeps its blocks forever, stalling the download near completion."},
         {"where": "internal/bt/storage/storage.go:98", "severity": "medium", "what": "The §4.5 worker pools are per-Storage semaphores plus goroutine-per-job (session/torrent.go:180), not process-wide bounded pools; N torrents get 4N disk and N*GOMAXPROCS/2 hash slots."},
         {"where": "internal/bt/tracker/loop.go:92", "severity": "medium", "what": "nextInterval has no floor; a tracker returning interval=1 is announced to every second."},
         {"where": "internal/bt/session/torrent.go:24", "severity": "low", "what": "The half-open limit (20) is per torrent instead of global; there is also no write deadline on peer connections (peer/conn.go:190), and D1 depth uses the lifetime-average rate (session/peers.go:28)."}
       ]},
 "prefer": "Y",
 "why": "Y has explicit, documented interfaces per package, a streaming storage design and a peer-aware picker that suit seeding, endgame and resume, plus a deterministic, adversarial test suite. Its defects (unrequested-block acceptance, missing endgame and pools, interval floor) are concentrated and fixable. X is more correct today but needs its whole-piece buffering and O(n) hot paths reworked."}
```
