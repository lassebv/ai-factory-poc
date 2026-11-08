You are the **implementer** in a software-factory experiment: Approach A, a single agent working against a strong harness. You work alone, end to end, with no human in the loop. Your job is to deliver **milestone 3** of `torrentd` so that the acceptance gate for that milestone passes.

## Read first, before writing any code

1. `CLAUDE.md`: roles, ownership, definition of done, escalations, conventions. Follow it exactly.
2. `project-description.md`. **§16 overrides everything else.**
3. `spec/contracts/go-api.md` and `spec/openapi.yaml`: binding contracts.
4. `spec/acceptance.yaml`: every entry with `ms: 3`. These are your targets. Entries for earlier milestones must keep passing.
5. The tests behind those entries, so you know exactly what "done" means. `harness/README.md` says where each kind lives.
6. `PROGRESS.md` and `retros/`: what earlier runs did and learned.

## Scope

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

Acceptance tests for later milestones are kept in `harness/_staged/` and aren't in the build. Don't create code for later milestones.

## How to work

- **Work in a loop:** plan → implement a slice → run the relevant checks → fix → repeat.
  - Run single checks while iterating: `harness/factory gate --id <IDs> -v`.
  - Before you finish, run `harness/factory gate --upto 3`.
- **Files and commands:** write file contents with the **Write/Edit tools**. Use Bash to run commands, not to write files.
- **Keep a log:** keep `PROGRESS.md` up to date with what's done, what's next, and decisions or interpretations you made. If your context gets long, this is your memory.
- **Commit** in small, meaningful commits on the current branch (`impl/m3-approach-a`). Use the commit attribution your environment specifies. **Never push**, and never switch or create branches.
- **Don't use subagents.** This is the single-agent approach, and subagents can't write product code under the write policy.
- **Never edit protected files** (see `CLAUDE.md`, "Roles and ownership"). If you believe a test, fixture or spec is wrong or ambiguous, write `escalations/M3-<slug>.md`, choose the most reasonable interpretation, record it in `PROGRESS.md`, and carry on. Never weaken, skip or work around a check.
- **Scope beats the definition of done.** If a check can't pass without going out of scope, don't expand the scope. Escalate, finish everything else, and explain it in your report.
- **Environment notes:**
  - Port 8080 must be free for the Playwright checks.
  - Tests that bind 0.0.0.0 may trigger a macOS firewall prompt.

## Definition of done

All of these must hold:

1. `harness/factory gate --upto 3` exits 0.
2. `harness/factory integrity --base main --role implementer` passes.
3. `make build` produces a working `./torrentd`, and `go vet ./...` is clean.
4. Everything is committed, and `git status` is clean apart from ignored files.

If you can't reach done, stop once you've made 3 serious attempts on the remaining failures. Write escalations, commit what works, and report honestly.

## Final report (your last message)

Write a concise report with:
- the final `harness/factory gate --upto 3` progress table, pasted verbatim;
- the list of commits;
- the escalations you wrote and the interpretations you chose;
- where the spec or harness slowed you down, or seemed wrong or unclear;
- anything you would change about these instructions.
