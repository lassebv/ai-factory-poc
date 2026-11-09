# M4 risk review (architect, Approach A+)

The scope is §15.4 + ADR-0016. The contracts are frozen in `internal/bt/{piece,choke,storage,peer,session}/contract.go`, `internal/store/contract.go` and `internal/api/contract.go`. The decisions are in ADR-0017 (data path, pools, choking), ADR-0018 (picker) and ADR-0019 (persistence, resume, lifecycle).

The principle throughout is to **make bad states unrepresentable or structurally impossible, rather than merely forbidden.** Each risk below says how the contract does that, and which contract test (CT) guards it.

## 1. Hostile or broken peers

| Risk | How the contract addresses it | Guard |
| --- | --- | --- |
| **Corrupt data** (a peer whose pieces always fail) | `PieceFailed` returns the contributors' **IPs**, not keys, so a peer that has already disconnected still gets its strike. 3 strikes → process-wide ban: closed everywhere, never dialled, refused before the handshake, persisted 24 h. | CT `PickerFailureContributors`, `SessionBansCorruptPeer`; BAN-01 |
| **The innocent peer gets banned too** (the BAN-01 livelock) | Piece affinity (a piece mostly from one peer) and single-source retry of failed pieces, so a second failure blames exactly one IP. See `escalations/M4-ban-innocent-strikes.md`. | CT `PickerPieceAffinity`, `PickerFailureContributors` |
| **A stalled peer holding blocks** (keep-alives only, known issue 1) | Every outstanding block has an owner and a request time. `Expire()` (≥ 1/s) takes blocks back after 20 s and sends a cancel, and the staller gets nothing new until its cooldown ends. Endgame duplicates the last blocks to up to 3 peers and cancels the losers. Re-requesting is first-class, so no later milestone has to undo a "never re-request" rule. | CT `PickerExpire`, `PickerEndgame`, `SessionStalledPeerDoesNotHang`; PICK-02 |
| **A slow peer** | Its pipeline depth follows its own measured rate (D1). In endgame its blocks are duplicated. Its upload queue is bounded (256 requests, 4 blocks read ahead). | CT `PipelineDepth`, `Meter` |
| **Unrequested or poisoned blocks** (known issue 3) | `Received` accepts a block only from a peer it was requested from (or a late answer to a released request). The read goroutine writes only after the actor has accepted, so unrequested bytes never touch the disk and never corrupt an in-progress piece. | CT `PickerOwnership`; POISON-01 |
| **Oversized messages and requests** | The peer layer rejects length prefixes > 16 KiB + 13 before allocating (M3). A request with `Length > 16 KiB` closes the connection (WIRE-03). Other invalid requests are ignored. Requests from choked peers and for pieces we don't have are not served. | peer CTs (M3); WIRE-03 |
| **Disconnect mid-transfer** | `RemovePeer` frees the peer's blocks but keeps its contributions. Accepted blocks still being written finish, and the piece is hashed only when all of its writes are done. Redial backoff of 30 s, doubling, prevents reconnect storms (FD-01). | CT `PickerRemovePeerFreesBlocks` |
| **Seed-to-seed connections and connections to ourselves** | Seeds are closed when we're seeding. Our own peer ID is refused. One connection per ip:port per torrent. | session doc |

## 2. Shutdown under load

- **Order** (§16.11, ADR-0015): `Close` and the HTTP drain start together. The `stopped` announces run in parallel with closing peers, bounded by 3 s. Then the session waits for disk writes in flight, does the final flush (fingerprint after the last write), and closes storages and then pools. `cmd` writes the clean marker after `Close` returns, then closes the DB.
- **In-flight requests and writes:** a read goroutine blocked in `WriteBlock` finishes its write. Pool `Close` drops queued, not-yet-started work and waits for running work. Storages are closed before the pools, so `VerifyPieceAsync` callbacks are always delivered (with `ErrClosed` if needed).
- **Shutdown during checking:** checking is cancellable, and a partial check doesn't change the saved bitfield.
- **Guards:** CT `PoolBoundsAndClose`, `SessionSeedFromDisk` (stopped announce on Close), and `btest.CheckGoroutines` in every M4 session CT; SHUT-01, TRK-01.

## 3. Resource bounds

| Resource | Bound | By construction |
| --- | --- | --- |
| Receive memory | ≈ 1 block per connection + 256 KiB per hash worker | Writes happen on the peer's own read goroutine (back-pressure). Hashing streams. |
| Upload memory | ≤ 4 × 16 KiB per peer | `UploadReadAhead`, freed by `OnPieceSent` |
| Goroutines | reader + writer + ≤ 1 helper per connection; exactly N pool workers | No goroutine per block or per piece (known issue 2). CT `StorageUsesSharedPools` |
| Disk / hash concurrency | 4 / GOMAXPROCS/2 process-wide | Storage runs its work on the shared pools itself, so callers can't bypass the bound |
| Connections | 50 per torrent, 20 half-open, one per address | session constants |
| File handles | LRU of 128 per storage (M3); dirty handles fsynced before eviction | storage contract |
| CPU (known issue 4) | No per-call scan or sort of all pieces | CT `PickerCost` (8× pieces must cost < 24× time); CPU-01 |
| Send queue | `peer.SendQueueLimit` (1024): a peer that doesn't read is closed | HAVE goes only to peers that lack the piece, so a seed gets none |

8 MiB pieces (MEM-01): no piece is ever buffered whole, neither when receiving, when hashing, nor when uploading.

## 4. Persistence after a crash

- **Fsync before bitfield:** the flush is capture → `Sync` → `Stat` → one transaction. `Sync` also covers handles the LRU evicted. (ADR-0019; CT `StorageUsesSharedPools` covers `Sync`.)
- **kill -9:** SQLite in WAL mode, synchronous=NORMAL, one transaction per flush (RES-05 `integrity_check`). The unclean path checks sizes only, so after a crash nothing is rechecked unnecessarily. Pieces since the last flush are downloaded again.
- **Tampering and missing files:** `PlanResume` is pure and table-tested for every clean/unclean, changed/missing/fresh-add case. `Stat` never creates files, so "Files missing" can't be masked by `Open` recreating them.
- **Startup restore:** desired state and stop reason are restored. C6 applies: a stopped torrent found incomplete goes to paused.
- **Guards:** CT `StoreTorrentRoundTrip`, `StoreSurvivesReopen`, `StoreBans`, `PlanResume`, `StatFingerprint`, `SessionPersistence` (restart, stopped and paused kept, no recheck when unchanged), `SessionFilesMissing`.

## 5. Contradictions and gaps in the spec and harness

1. **G18 gaps** (pause on paused or error, stop-seeding on checking, `startPaused` over complete data): interpreted as idempotent no-ops. Checking counts as not complete. Complete data with `startPaused` → `stopped`/`user`. See `escalations/M4-action-validity-gaps.md` and ADR-0019.
2. **ADR-0006 vs BAN-01:** see `escalations/M4-ban-innocent-strikes.md`. Mitigated in the picker. The human may want a "no strike for shared pieces" rule.
3. **Request timeout:** the spec doesn't give a value. 20 s was chosen; it fits PICK-02's 45 s together with endgame.
4. **CHOKE-01 vs Fill:** §4.1 says "re-evaluated every 10 s". Filling free slots between rechokes is an interpretation: without it, every new leecher waits up to 10 s. Fill never takes away a slot that has been given out.
5. **Who writes the clean marker:** `cmd`, after `Session.Close` (ADR-0019). The session itself doesn't know about `store.BeginSession`.
6. **`btest` is product-owned under the write policy** (it isn't a `contract.go`). The implementer can edit it. Its fakes are small. Any change to it should be reviewed in the integrity diff.

## 6. Integration risks for the single implementer

- **Peer goroutines vs the actor (deadlock):** the read goroutine waits for the actor's accept decision, and the actor must never block sending back to it. Use a reply channel with a buffer of 1 per peer, or a non-blocking send. When a peer closes, its pending writes must still report to the actor, or the "writes in flight" counter leaks and the piece is never hashed.
- **Ordering hazards M3 already hit:**
  - send our bitfield before `interested`;
  - register the peer with the picker before processing its bitfield;
  - hash only after the piece's writes have completed.
- **The M3 code was adapted mechanically to the new signatures.** The M3 `picker.go` still has M3 semantics, and `piece_test.go` and `session_test.go` contain M3 expectations (for example `TestPickerReceivedUnrequestedStartsPiece`). These now contradict the contract. The implementer must rewrite them, not "fix" the contract tests.
- **Real-time tests** (CHOKE-01 ≈ 40 s, BAN-01 × 3, RES-05 ≈ 25 s) make the gate slow. Run `--id` subsets while iterating.
