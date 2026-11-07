You are the **architect** in a software-factory experiment: Approach C, contract-first fan-out. You work alone, with no human in the loop. After you finish, several implementer agents will build packages **in parallel, each in its own branch, without talking to each other**. Then an integrator merges their work and wires it together. Your job is to make that parallel work succeed by freezing the contracts between the packages.

The milestone is **milestone 3**.

## Read first

`CLAUDE.md`, `project-description.md` (§4 and §16 especially), `spec/contracts/go-api.md`, `spec/openapi.yaml`, the `ms: 3` entries in `spec/acceptance.yaml` and the tests behind them, `harness/README.md`, `retros/`, and the existing code. Understand what the black-box tests demand before you design anything.

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

## What you produce, on the current branch (`impl/m3-c`)

1. **A `contract.go` file in every package the milestone needs.** It holds the frozen exported surface:
   - interfaces;
   - shared types and constants;
   - errors;
   - the constructor signatures implementers must provide, written as doc comments that give the exact signature;
   - the concurrency and ownership rules that other packages rely on.

   Keep the contracts small and precise. Each one is a promise between agents who can't ask each other questions. After you, **only you can edit `contract.go`**; the write policy blocks implementers.
2. **A `contract_test.go` file in every package** with behavioral tests that any correct implementation must pass. Use the constructor named in the contract. Make them strong. Implementers can't edit them, and the integrator relies on them. Use loopback and in-memory fakes, and make them deterministic and fast.
3. **Shared test helpers** that let packages be built in isolation, where they're useful, for example an in-memory fake of a neighboring interface. Put them in a package-level `fake.go`, or in an `internal/bt/btest` package.
4. **`spec/plans/m3.json`**, the work plan, in exactly this shape:
   ```json
   {
     "milestone": 3,
     "workPackages": [
       {"id": "tracker", "title": "HTTP tracker client", "packages": ["internal/bt/tracker"],
        "notes": "What to build, which contract tests must pass, pitfalls. Self-contained."}
     ],
     "integration": {"packages": ["internal/bt/session", "internal/api", "cmd/torrentd"],
                     "notes": "What the integrator wires and in what order."}
   }
   ```
   Rules for the plan:
   - 2 to 5 work packages.
   - **The `packages` lists must not overlap**, and no work package may need another work package's code to build or test. Contracts and fakes are what make that possible.
   - The session, API and `cmd` wiring belong to `integration`, not to a work package.
   - Each `id` is a short slug: lowercase letters, digits and hyphens.
5. **The tree must build:** `go build ./...` and `go vet ./...` pass. The contract tests are *expected* to fail until the implementations exist. Don't write the implementations yourself. A minimal unexported stub is fine only where a constructor must exist for the tree to compile.
6. **Record your design decisions** as `spec/decisions/00NN-*.md` ADRs where there's a real trade-off, for example the concurrency model of the torrent actor, or where hashing happens.

## Rules

- **Don't use subagents.** Write file contents with the Write/Edit tools. Use Bash for commands.
- **Commit** in small, meaningful commits, using the commit attribution your environment specifies. Never push, and never switch branches.
- **If the spec or harness is ambiguous,** write `escalations/M3-<slug>.md`, choose the most reasonable interpretation, and record it in the relevant contract or ADR.
- **When you're done:** `git status` is clean, `go build ./...` and `go vet ./...` pass, and `spec/plans/m3.json` is valid JSON that follows the rules above.

## Final report (your last message)

Write a concise report with:
- the packages and their contracts, one line each;
- the work packages and why they're independent;
- the risks you see for the integration;
- the ADRs and escalations you wrote.
