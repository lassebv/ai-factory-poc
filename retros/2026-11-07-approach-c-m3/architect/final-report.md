# Architect report: milestone 3 (Approach C)

The contracts, the plan, four ADRs and two escalations are committed on `impl/m3-c` (4 commits) and the tree is clean. `go build ./...`, `go vet ./...`, `factory integrity --role architect` and `lint-clock` all pass, and `spec/plans/m3.json` passes the launcher's validation.

Two things are expected to stay red until the code exists:
- **Contract tests:** every new contract test currently panics on a stub, as intended. I checked that they compile and fail in under a second instead of hanging.
- **staticcheck:** it reports dead code after calls that always panic, and unused `Bitfield` fields. Both disappear once the implementations replace the stubs.

I haven't proven the contract tests can pass, because no implementation exists yet. A wrong test is now frozen and only I can change it, so the first real check is when the implementers run them.

## Packages and contracts
Each constructor is an exported one-line wrapper over an unexported function, so the signature is fixed in `contract.go` and the implementer only writes the function behind it (ADR-0012). A `stub.go` file keeps the tree compiling until the implementer deletes it.

- **`internal/bt/piece`:** the `Bitfield` type, the block layout, `PipelineDepth` (5–250 outstanding requests, D1) and the `Picker`. The picker is not concurrency-safe; the torrent goroutine owns it. Its order is fixed: pieces already in progress first, then rarest-first with ties broken by lowest index, no random-first and no endgame at M3.
- **`internal/bt/storage`:** `Open` and a `Storage` that is safe to call from many goroutines:
  - writes blocks as they arrive and hashes pieces by reading them back from disk;
  - creates sparse `.part` files and renames each one when all its pieces verify;
  - never puts pad files on disk;
  - keeps at most 128 files open;
  - limits concurrent disk work and hashing internally (these stand in for the §4.5 worker pools).
- **`internal/bt/peer`:** handshake and message encoding with size checks done before reading the payload (bitfield exempt), and a `Conn` with separate read and write goroutines. `Send` never blocks, there are keep-alives and an idle timeout on the injected clock, and the close and error rules are spelled out.
- **`internal/bt/tracker`:** an HTTP announce `Client`, an `Announcer` for BEP 12 tiers, and the per-torrent announce `Loop` (started, regular, completed, stopped; retry backoff; intervals on the injected clock).
- **`internal/bt/session`** (integration): the `Session` interface (add, list, get, peer port, peer ID, close), the `Status` snapshot, the states and the errors.
- **`internal/api`** (integration): the `TorrentService` interface, the JSON shape of a torrent, the error mapping, and a route table that avoids the `ServeMux` panic. The only product-code change outside a contract file is one new field, `Options.Torrents`, in `server.go`.

Each package has a `contract_test.go` that uses only loopback connections and in-memory fakes. The session test is a full in-process download: a fake HTTP tracker, a fake seeder that sends one corrupted piece, a goroutine-leak check, and a check that the stopped announce arrives in time.

## Work packages: `tracker`, `peer`, `piece`, `storage`
They are independent because each one depends only on code already merged (`bencode`, `metainfo`, `clock`) and never on another work package. That's why `peer` carries bitfields as raw bytes and `piece.Block` repeats the wire field types instead of importing `peer`. Each package's tests bring their own fakes, so there's no shared helper package (which would also have needed 80% coverage). The `session`, `api` and `cmd/torrentd` wiring is left to the integrator, with step-by-step notes in the plan.

## Integration risks
- **Stopped announce timing (TRK-01):** the stopped announce must arrive within 3.5 s of SIGTERM. The session's close has to run at the same time as the HTTP shutdown, not after it.
- **Topping up requests:** after every received block and every disconnect, the session must refill the request queue of every unchoked peer. Otherwise blocks freed by a dead peer are never re-requested (WIRE-02, WIRE-04).
- **40 garbage peers (WIRE-04):** the session must connect to at least 20 of them within 30 s, so dials need to run concurrently with a short handshake timeout.
- **Open files (STOR-06):** `lsof` counts every file open under the download dir, so nothing outside storage may hold file handles.
- **Consistent snapshots:** when a torrent reports `seeding`, `completedBytes` must already equal the full size in that same snapshot.
- **Session size:** the session is the largest piece of work. It must meet 80% coverage and the 800-line file limit.
- **SP-01** needs Docker running.

## ADRs and escalations
- **ADR-0012:** the contract file pattern, and in-order piece selection at M3.
- **ADR-0013:** blocks go to disk on arrival, pieces are verified from disk, and the worker pools are implemented as concurrency limits inside storage.
- **ADR-0014:** the tracker package owns the announce loop.
- **ADR-0015:** one goroutine per torrent, published snapshots, the shutdown order, and how the peer port is bound.
- **`escalations/M3-piece-order-vs-random-first.md`:** `TestPartFiles` (STOR-02) only passes with in-order piece selection, because the fake peer blocks its whole queue. That contradicts §4.1's random-first, so it will block M4. I recommend the verifier change the fake peer to stall only the affected pieces. The same file notes that the WIRE-04 text says 200 peers while the test uses 40.
- **`escalations/M3-shutdown-order.md`:** §6.2 runs the HTTP shutdown before the stopped announces, which can't be reconciled with the TRK-01 budget. I chose to run them at the same time; the human should confirm or amend §6.2.
