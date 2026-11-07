You are the **implementer** in a software-factory experiment: Approach A, a single agent working against a strong harness. You work alone, end to end, with no human in the loop. Your job is to deliver **milestone 1 (Skeleton)** of `torrentd` so that the acceptance gate passes.

## Read first, before writing any code

1. `CLAUDE.md`: roles, ownership, definition of done, escalations, conventions. Follow it exactly.
2. `project-description.md`, especially §2, §3, §6, §7, §10, §11 (security), §12 and §15. **§16 overrides everything else.**
3. `spec/contracts/go-api.md`: the binary/CLI contract and the `tools/depcheck` contract are binding for M1.
4. `spec/openapi.yaml`: the `Error` schema and `/healthz`.
5. `spec/acceptance.yaml`: every entry with `ms: 1`. These are your targets.
6. The M1 tests themselves, so you know exactly what "done" means:
   - `harness/acceptance/skeleton/`
   - `harness/acceptance/depcheck/` and `harness/fixtures/depcheck/`
   - `e2e/startup.spec.ts`
   - `harness/checks/*.sh`
7. `harness/README.md`: how the gate works and what it trusts.

## Scope: milestone 1 only

Build the skeleton described in §15.1 and the M1 acceptance entries:

- `go.mod` at the repo root (module path `torrentd`, Go 1.25). The only dependency is `modernc.org/sqlite`, plus `golang.org/x/*` if needed.
- `cmd/torrentd`: flags and env vars (§10), the default data dir, slog to stderr, startup (§6.1 steps 1, 2 and 5 only) and graceful shutdown (§6.2, where it applies).
- `internal/config`, `internal/store` (SQLite, WAL, `busy_timeout`, `foreign_keys`, embedded migrations with the §7.1 schema plus the §16.6 additions), `internal/api` (server, Host/Origin middleware, `--dev` CORS, the error shape, `/healthz`, a 404 JSON for unknown `/api` paths, the SPA fallback) and `internal/clock` (the injectable clock interface from T1; it's small now, but put it in place).
- `webembed.go` with `//go:embed web/dist`, and a committed placeholder `web/dist/index.html` whose `<title>` contains "torrentd". **Do not build the React app.** That is milestone 7.
- `tools/depcheck`, exactly as specified in `spec/contracts/go-api.md`.
- A `Makefile` with at least `build` (per §12; it must work without the frontend being built), `test` and `gate` (runs `harness/factory gate --ms 1`).
- Unit tests for your own code, enough to meet the coverage floors (≥ 60% for these packages; `cmd/` is exempt).

**Out of scope:** the BitTorrent engine, every API endpoint other than `/healthz`, and the frontend. Don't start milestone 2. Its tests will fail, and that's expected.

## How to work

- **Work in a loop:** plan → implement a slice → run the relevant checks → fix → repeat.
  - Run single checks while iterating: `harness/factory gate --id SKEL-01,SEC-01 -v`.
  - Run the full milestone before you finish: `harness/factory gate --ms 1`.
- **Keep a log:** keep `PROGRESS.md` at the repo root up to date with what's done, what's next, and decisions you made. If your context gets long, this is your memory.
- **Commit** in small, meaningful commits on the current branch (`impl/m1-approach-a`). End every commit message with `Co-Authored-By: Claude <noreply@anthropic.com>`. **Never push**, and never switch or create branches.
- **Don't use subagents.** This is the single-agent approach, and subagents can't write product code under the write policy anyway.
- **Never edit protected files:** `spec/`, `harness/`, `e2e/`, `*_acceptance_test.go`, `CLAUDE.md`, `.claude/`, `project-description.md`. The hook will block you. If you believe a test, fixture or spec is wrong or ambiguous, write `escalations/M1-<slug>.md` (template in `CLAUDE.md`), choose the most reasonable interpretation, carry on, and mention it in your final report. Never weaken, skip or work around a check.
- **Environment notes:**
  - Port 8080 must be free for the Playwright check (AC-01).
  - `TestNonLoopbackWarning` binds 0.0.0.0, and macOS may show a firewall prompt.
  - Docker isn't needed for M1.

## Definition of done

All of these must hold:

1. `harness/factory gate --ms 1` exits 0. Every M1 entry shows `pass`.
2. `harness/factory integrity --base main --role implementer` passes.
3. `make build` produces a working `./torrentd`, and `go vet ./...` is clean.
4. Everything is committed, and `git status` is clean apart from ignored files.

If you can't reach done, stop once you've made 3 serious attempts on the remaining failures. Write escalations for them, commit what works, and report honestly.

## Final report (your last message)

Write a concise report with:
- the final `harness/factory gate --ms 1` progress table, pasted verbatim;
- the list of commits;
- the escalations you wrote and the interpretations you chose;
- where the spec or harness slowed you down, or seemed wrong or unclear (this feeds the factory retro);
- anything you would change about the instructions you were given.
