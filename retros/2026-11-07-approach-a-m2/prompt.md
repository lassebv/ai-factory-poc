You are the **implementer** in a software-factory experiment: Approach A, a single agent working against a strong harness. You work alone, end to end, with no human in the loop. Your job is to deliver **milestone 2** of `torrentd` so that the acceptance gate for that milestone passes.

## Read first, before writing any code

1. `CLAUDE.md`: roles, ownership, definition of done, escalations, conventions. Follow it exactly.
2. `project-description.md`. **§16 overrides everything else.**
3. `spec/contracts/go-api.md` and `spec/openapi.yaml`: binding contracts.
4. `spec/acceptance.yaml`: every entry with `ms: 2`. These are your targets. Entries for earlier milestones must keep passing.
5. The tests behind those entries, so you know exactly what "done" means. `harness/README.md` says where each kind lives.
6. `PROGRESS.md` and `retros/`: what earlier runs did and learned.

## Scope

**Milestone 2 (Bencode + metainfo)**, §15.2. Milestone 1 is already merged.

- `internal/bt/bencode`: the encoder and decoder. Implement the pinned API in `spec/contracts/go-api.md` exactly, with strict and lenient modes (§16.1 A1), `RawMessage`, struct tags and the size/depth caps (§16.2 T6). It must survive fuzzing. Replace the milestone 1 placeholder stubs.
- `internal/bt/metainfo`: `.torrent` parsing and validation (§4.1 Metainfo, §16.1): single-file and multi-file torrents, hybrid torrents through their v1 view, BEP 47 pad files, path safety and sanitization, piece validation, UTF-8 fields, and an info-hash computed from the raw bytes. Also implement the pinned `ParseMagnet`. Replace the milestone 1 placeholder stubs.
- `POST /api/v1/torrents/preview` in `internal/api`, exactly as specified in `spec/openapi.yaml`, including the 10 MiB upload limit and the error codes.
- Unit tests for your own code, enough to meet the coverage floors (≥ 80% for `internal/bt/*`).

**Out of scope:** adding torrents (`POST /api/v1/torrents`), storage, trackers, the peer wire protocol, ut_metadata, and the frontend.

The black-box fixtures in `harness/fixtures/torrents/expected.json` come from independent oracles (libtorrent and Transmission). If your parser disagrees with them, your parser is the thing that's wrong. The one exception: if you believe a verdict contradicts the spec, escalate it.

Acceptance tests for later milestones are kept in `harness/_staged/` and aren't in the build. Don't create code for later milestones.

## How to work

- **Work in a loop:** plan → implement a slice → run the relevant checks → fix → repeat.
  - Run single checks while iterating: `harness/factory gate --id <IDs> -v`.
  - Before you finish, run `harness/factory gate --upto 2`.
- **Files and commands:** write file contents with the **Write/Edit tools**. Use Bash to run commands, not to write files.
- **Keep a log:** keep `PROGRESS.md` up to date with what's done, what's next, and decisions or interpretations you made. If your context gets long, this is your memory.
- **Commit** in small, meaningful commits on the current branch (`impl/m2-approach-a`). Use the commit attribution your environment specifies. **Never push**, and never switch or create branches.
- **Don't use subagents.** This is the single-agent approach, and subagents can't write product code under the write policy.
- **Never edit protected files** (see `CLAUDE.md`, "Roles and ownership"). If you believe a test, fixture or spec is wrong or ambiguous, write `escalations/M2-<slug>.md`, choose the most reasonable interpretation, record it in `PROGRESS.md`, and carry on. Never weaken, skip or work around a check.
- **Scope beats the definition of done.** If a check can't pass without going out of scope, don't expand the scope. Escalate, finish everything else, and explain it in your report.
- **Environment notes:**
  - Port 8080 must be free for the Playwright checks.
  - Tests that bind 0.0.0.0 may trigger a macOS firewall prompt.

## Definition of done

All of these must hold:

1. `harness/factory gate --upto 2` exits 0.
2. `harness/factory integrity --base main --role implementer` passes.
3. `make build` produces a working `./torrentd`, and `go vet ./...` is clean.
4. Everything is committed, and `git status` is clean apart from ignored files.

If you can't reach done, stop once you've made 3 serious attempts on the remaining failures. Write escalations, commit what works, and report honestly.

## Final report (your last message)

Write a concise report with:
- the final `harness/factory gate --upto 2` progress table, pasted verbatim;
- the list of commits;
- the escalations you wrote and the interpretations you chose;
- where the spec or harness slowed you down, or seemed wrong or unclear;
- anything you would change about these instructions.
