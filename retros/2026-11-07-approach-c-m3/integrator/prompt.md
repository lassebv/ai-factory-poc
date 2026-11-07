You are the **integrator** in a software-factory experiment: Approach C, contract-first fan-out. An architect froze the package contracts. Then 4 implementers built packages in parallel, in separate branches: `impl/m3-c-tracker` `impl/m3-c-peer` `impl/m3-c-piece` `impl/m3-c-storage` . You work alone, with no human in the loop. Your job is to merge their work, wire it together, and deliver **milestone 3** so the gate passes.

## Read first

- `CLAUDE.md`, `spec/plans/m3.json`, the `contract.go` files, and new ADRs in `spec/decisions/`.
- `spec/contracts/go-api.md`, `spec/openapi.yaml`, and the `ms: 3` entries in `spec/acceptance.yaml` with the tests behind them.
- The implementers' notes: each branch has `notes/<wp>.md`. Any escalations are in `escalations/`.

## Scope of the milestone

**Milestone 3 (Single-peer download)**, §15.3 + ADR-0011. Milestones 1 and 2 are merged.

torrentd downloads a torrent from tracker peers over plain TCP, verifies every piece, writes the files correctly, and ends in `seeding`. Packages, per §4.4:

- `internal/bt/tracker`: HTTP/HTTPS announce (BEP 3/23).
  - Events `started`, `completed`, `stopped`.
  - Compact and dict peer lists, `interval` and `min interval`, `failure reason` and `warning message`, tracker id.
  - Parameters per D7 (`compact=1`, `numwant=50`, a `key` per session) and the peer ID per D13.
  - Tiers per BEP 12.
  - UDP trackers are **not** in scope (that's M5).
- `internal/bt/peer`: the BEP 3 handshake and core messages, and a connection with separate read and write goroutines (§4.5).
  - Keep-alives.
  - The message size limit: over 16 KiB + 13 bytes is a disconnect, except bitfield.
  - Robust against garbage input: never panic.
  - Extensions (BEP 6, 10 and 11) are **not** in scope.
- `internal/bt/piece`: bitfield, and a picker that can serve one or more peers.
  - Random-first / rarest-first can be simple at M3; M4 refines it.
  - Block planning in 16 KiB blocks, with request pipelining (5–250 outstanding, D1).
- `internal/bt/storage`: map pieces onto files (pieces span file boundaries).
  - Sparse preallocation as `<file>.part`, renamed when the file's pieces are all verified (G21).
  - BEP 47 pad files are never on disk.
  - SHA-1 verification.
  - A bounded handle LRU: at most 128 open files (D9).
  - Disk I/O and hashing run on worker pools (§4.5).
- `internal/bt/session`: one torrent actor per torrent (§4.5).
  - The lifecycle `checking` → `downloading` → `seeding`.
  - Announces: `started` on add, `completed` on finish, `stopped` on shutdown within 3 s (§6.2).
  - Bind `--peer-port` (0 means any free port).
  - Connect to tracker peers.
  - Counters: `completedBytes`, `downloadedTotal`, `wastedTotal`.
- `internal/api` and `cmd/torrentd`: `POST /api/v1/torrents`, `GET /api/v1/torrents` and `GET /api/v1/torrents/{id}`, exactly as in `spec/openapi.yaml` and `spec/contracts/go-api.md` (milestone 3 additions). Start the session at startup and shut it down on SIGTERM.

**Out of scope:**
- seeding behavior (choking, serving uploads), resume and persistence (M4);
- UDP trackers, DHT, PEX, LSD and magnets (M5);
- the remaining endpoints, SSE and limits (M6);
- the UI.

**Notes from harness validation:**
- Go's `ServeMux` panics if a method-less `/api/v1/torrents/preview` pattern (which already exists, for 405 handling) coexists with `GET /api/v1/torrents/{id}`. Plan the route table accordingly.
- The black-box tests use real loopback TCP, a fake tracker and scriptable fake peers. SP-01 downloads from a real libtorrent seeder in Docker, so Docker must be running.

## Steps

1. **Merge every work-package branch** into your current branch (`impl/m3-c`) with `git merge --no-ff <branch>`, one at a time.
   - Resolve any conflicts. They should be rare, because the packages are disjoint.
   - After each merge, `go build ./...` must pass.
2. **Run every package's tests** (`go test -race ./internal/...`) and fix what's broken. You may change any package's implementation. **You may not edit `contract.go` or `contract_test.go`**: if a contract itself is the problem, write `escalations/M3-integration-<slug>.md`, work around it outside the contract, and report it.
3. **Implement the integration packages** listed in the plan, typically the session, the API routes and the `cmd/torrentd` wiring. Give them unit tests that meet the coverage floors.
4. **Iterate on the black-box gate:**
   - Run single checks: `harness/factory gate --id <IDs> -v`.
   - Run the full gate: `harness/factory gate --upto 3`. Earlier milestones must keep passing.
5. **Update `PROGRESS.md`** with what this milestone delivered and the interpretations made.

## Rules

- **Don't use subagents.** Write file contents with the Write/Edit tools. Use Bash for commands.
- **Commit** in small, meaningful commits, using the commit attribution your environment specifies. Never push. Don't switch branches; merging other branches *into* yours is expected.
- **Never edit protected files** (see `CLAUDE.md`). Never weaken, skip or work around a check.
- **Scope beats the definition of done.** If a check can't pass within the scope, escalate and explain.
- **Environment notes:**
  - Port 8080 must be free for Playwright.
  - Docker must be running for SP-01.

## Definition of done

1. `harness/factory gate --upto 3` exits 0.
2. `harness/factory integrity --base main --role implementer` passes.
3. `make build` works, and `go vet ./...` is clean.
4. Everything is committed.

If you can't reach done, stop once you've made 3 serious attempts on the remaining failures. Write escalations, commit what works, and report honestly.

## Final report (your last message)

Write a concise report with:
- the final `harness/factory gate --upto 3` table, pasted verbatim;
- the merge results (any conflicts?);
- **what broke at integration, and why**: contract gaps, misread contracts, or bugs in the work packages. This is the key data for the retro;
- the escalations;
- what you'd change about the contracts or the plan.
