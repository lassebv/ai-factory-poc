You are the **architect** in a software-factory experiment: Approach A+, an architect first and then a single implementer. You work alone, with no human in the loop. After you finish, **one** implementer agent builds every package and the integration against your contracts. It can't edit them; it can only escalate. Your job is to make that work succeed and be robust, by freezing clear contracts and finding the risks before any code exists.

The milestone is **milestone 4**.

## Read first

`CLAUDE.md`, `project-description.md` (§4 and §16 especially), `spec/contracts/go-api.md`, `spec/openapi.yaml`, the `ms: 4` entries in `spec/acceptance.yaml` and the tests behind them, `harness/README.md`, `retros/`, and the existing code. Understand what the black-box tests demand before you design anything.

## Scope of the milestone

**Milestone 4 (Swarm engine)**, §15.4 + ADR-0016. Milestones 1–3 are merged. You're building on the M3 engine (`internal/bt/{tracker,peer,piece,storage,session}`), which was built with frozen `contract.go` files. Those contracts describe M3. **Evolving them for M4 is expected.** Whoever owns the contracts in your setup changes them deliberately, and records each change in an ADR.

What M4 adds:

- **Seeding.**
  - The peer port serves incoming connections for active torrents.
  - Requests are answered from storage.
  - `HAVE` is sent for newly verified pieces.
  - Uploads count in `uploadedTotal`.
  - Adding a torrent whose data already exists verifies it (`checking`) and goes to `seeding` (§5.3).
- **Choking (§4.1):**
  - tit-for-tat with 4 unchoke slots by rate, re-evaluated every 10 s;
  - an optimistic unchoke rotated every 30 s;
  - seeding mode unchokes by upload rate;
  - anti-snubbing after 60 s without data.
- **The picker (§4.1, A5):**
  - random-first for the first 4 pieces, then rarest-first, using availability from bitfields and `have` messages;
  - strict priority classes, ready for M6;
  - **endgame mode:** request the remaining blocks from all peers and cancel duplicates;
  - per-peer block ownership, plus a request timeout or re-request, so a stalled peer can't hold blocks forever.
- **Adaptive request pipelining (D1):** clamp(rate × 2 s / 16 KiB, 5, 250).
- **Hash failures and banning (§4.1, G10, ADR-0006):**
  - record which peers contributed to each piece, and give each contributor a strike on failure;
  - ban an IP at 3 strikes (24 h expiry, persisted in `banned_peers`), then disconnect it and never reconnect to it;
  - a peer that corrupts everything must never stall the download.
- **Persistence and fast resume (§6.1 steps 3–4, §6.2, §7, §16.3 C3, G8, ADR-0002):**
  - torrents, `desired_state`, `stop_reason`, the bitfield, the resume fingerprint (per-file size + mtime) and lifetime totals are stored, and flushed every 10 s and at shutdown;
  - on startup, a clean shutdown with matching fingerprints means no recheck; changed files mean a recheck; an unclean shutdown means size checks only;
  - all files missing means `error` "Files missing".
- **Lifecycle (§5, ADR-0016):**
  - `POST …/pause`, `…/resume` and `…/stop-seeding`, with the G18 rules;
  - stop seeding sends `stopped` announces, closes peers, and moves the torrent to `stopped` with `stopReason: user`;
  - resume seeding goes back to `seeding`.
- **Concurrency and resources (§4.5):**
  - bounded, process-wide disk and hash worker pools (defaults 4 and GOMAXPROCS/2), not one goroutine per block;
  - no leaked goroutines or connections; there's an in-repo goroutine-leak helper for tests;
  - bounded memory with large pieces;
  - `-race` clean.
- **Shutdown (§16.11):** the `stopped` announces start at the same time as the HTTP drain.

**Out of scope:** UDP trackers, DHT, PEX, LSD and magnets (M5); seeding limits, queueing, settings, SSE and the remaining endpoints (M6); the UI (M7); BEP 6, IPv6 announces and interop beyond SEED-02 (M8).

**Known issues in the M3 code you inherit** (from independent reviews and hidden tests):
1. There's no endgame and no request timeout: a peer that holds blocks and only sends keep-alives stalls the download.
2. Disk writes and hashing use one goroutine per block, behind per-torrent semaphores, instead of §4.5's process-wide bounded pools.
3. `piece.Picker.Received` accepts blocks that were never requested from that peer.
4. `Pick` sorts every needed piece on each call, and `fillAll` runs on every received block.

**Notes:**
- **CHOKE-01** watches real time (about 40 s).
- **SEED-02** needs Docker (a libtorrent leecher).
- **BAN-01** puts the good peer on `[::1]`. Tracker dict peer lists may contain IPv6 literals.

## What you produce, on the current branch (`impl/m4-aplus`)

1. **A `contract.go` file in every package the milestone needs.** It holds the frozen exported surface:
   - interfaces;
   - shared types and constants;
   - errors;
   - the constructor signatures implementers must provide, written as doc comments that give the exact signature;
   - the concurrency and ownership rules that other packages rely on.

   Keep the contracts small and precise. Each one is a promise between agents who can't ask each other questions. After you, **only you can edit `contract.go`**; the write policy blocks implementers.
2. **A `contract_test.go` file in every package** with behavioral tests that any correct implementation must pass. Use the constructor named in the contract. Make them strong. Implementers can't edit them, and the integrator relies on them. Use loopback and in-memory fakes, and make them deterministic and fast.
3. **Shared test helpers** that let packages be built in isolation, where they're useful, for example an in-memory fake of a neighboring interface. Put them in a package-level `fake.go`, or in an `internal/bt/btest` package.
4. **A risk review**, written to `spec/plans/m4-risks.md`. For each risk, say how the contracts or contract tests address it. Cover at least:
   - hostile or broken peers: corrupt data, peers that stall while holding blocks, slow peers, unrequested or oversized messages, peers that disconnect mid-transfer;
   - shutdown under load and with in-flight requests;
   - resource bounds: memory per piece, goroutines, connections, file handles;
   - persistence after a crash;
   - contradictions or gaps in the spec and harness.

   **Prefer contracts that make bad states impossible over contracts that merely forbid them.** Avoid freezing in decisions that a later milestone will have to undo (for example: no endgame, or a block can never be re-requested).
5. **The tree must build:** `go build ./...` and `go vet ./...` pass. The contract tests are *expected* to fail until the implementations exist. Don't write the implementations yourself. A minimal unexported stub is fine only where a constructor must exist for the tree to compile.
6. **Record your design decisions** as `spec/decisions/00NN-*.md` ADRs where there's a real trade-off, for example the concurrency model of the torrent actor, or where hashing happens.

## Rules

- **Don't use subagents.** Write file contents with the Write/Edit tools. Use Bash for commands.
- **Commit** in small, meaningful commits, using the commit attribution your environment specifies. Never push, and never switch branches.
- **If the spec or harness is ambiguous,** write `escalations/M4-<slug>.md`, choose the most reasonable interpretation, and record it in the relevant contract or ADR.
- **When you're done:** `git status` is clean, `go build ./...` and `go vet ./...` pass, and the risk review is committed.

## Final report (your last message)

Write a concise report with:
- the packages and their contracts, one line each;
- the work packages and why they're independent;
- the risks you see for the integration;
- the ADRs and escalations you wrote.
