# ADR-0017: M4 engine data path, process-wide worker pools, choking

- **Status:** Accepted (2026-11-08, architect, Approach A+ on M4)
- **Spec:** §4.1 (Choking, Storage), §4.5, §15.4 · **Acceptance:** SEED-01/02, SWARM-01, CHOKE-01, WIRE-03, MEM-01, FD-01, CPU-01, RACE-01
- **Contracts:** `internal/bt/storage/contract.go` (Pools, VerifyPieceAsync, Sync), `internal/bt/peer/contract.go` (AcceptHandshake, OnPieceSent), `internal/bt/choke/contract.go` (new), `internal/bt/session/contract.go` (data path)

## Context

The M3 engine ran every disk write and hash in its own goroutine behind per-torrent semaphores (known issue 2). §4.5 asks for process-wide bounded pools, MEM-01 asks for < 150 MiB with 8 MiB pieces from 16 peers, and the actor must never block. M4 also adds uploading, which brings a second memory risk: blocks read from disk and queued for slow peers.

## Decision

1. **Process-wide pools live in `storage`.** `storage.Pools{Disk, Hash}` is created once by the Session and passed to every `storage.Open`. A `Pool` is a fixed set of N worker goroutines and a FIFO queue. `Run` blocks the caller; `Submit` doesn't. Storage methods run on the pools themselves, so a caller cannot bypass the bound. The hash worker reads its piece directly, not through Disk, so the two pools can't deadlock.
2. **Who writes a block: the peer's own read goroutine.** Flow: read goroutine → actor (`Picker.Received` decides) → read goroutine → `WriteBlock` (blocks on the disk pool) → "written" event → actor. Blocking the read goroutine is TCP back-pressure on that one peer. In-flight receive memory is about one block per connection. The actor never waits. A piece is hashed (`VerifyPieceAsync`) only after it is pending **and** all of its accepted writes have completed. That fixes M3's hash-after-write ordering hazard by construction. Unrequested data never reaches the disk, because the picker decides first (POISON-01).
3. **Hashing streams.** `VerifyPiece` uses at most `MaxHashBuffer` (256 KiB) whatever the piece length, so memory is bounded by hash workers × 256 KiB.
4. **Upload memory is bounded at the source.** Each peer has at most `MaxUploadQueue` (256) request descriptors and at most `UploadReadAhead` (4) blocks read and queued in its `peer.Conn`. `peer.Config.OnPieceSent` reports a block once it is actually written, which frees a read-ahead slot and counts `uploadedTotal` as sent bytes, not queued bytes. A slow leecher therefore costs at most 64 KiB of block data.
5. **Choking is a pure package (`choke`).** `Rechoke(now, peers, seeding)` runs every 10 s and `Fill` runs on connect, disconnect and interest changes. The time comes in as a parameter, so the algorithm is deterministic under test (T1/T2), and CHOKE-01 still observes real time (ADR-0016). `Fill` gives a newly interested peer a free slot at once, so SEED-01 doesn't wait 10 s. Rates come from `choke.Meter` (a 5 s window), which also feeds `PipelineDepth` (D1).
6. **Incoming connections:** `peer.AcceptHandshake` reads the remote handshake first. It answers only if the lookup accepts the torrent, which must be active and not checking. Banned IPs are refused before the handshake.

## Consequences

- No goroutine per block or per piece anywhere. Goroutines per connection: reader, writer and at most one helper.
- The torrent actor's event channel must not become the bottleneck at CPU-01 rates. The actor does O(1) work per block (see ADR-0018).
- The pools are shared, so a torrent being checked competes with downloads for hash workers. G11 ("one check at a time") is M6 and can be layered on top without changing this contract.
- `btest.CheckGoroutines` (in-repo, goleak-style) is the leak check for the contract tests (§4.5).
