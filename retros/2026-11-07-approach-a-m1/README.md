# Retro: Approach A (single agent + harness) on milestone 1

**Date:** 2026-11-07
**Branch:** `impl/m1-approach-a`, merged into `main`
**Factory version:** v1.0 (`28efc48`), fixed in v1.1 (this commit)

## Setup

- **Session:** one headless Claude Code session (`claude-opus-5-5`, `--permission-mode auto`, `FACTORY_ROLE=implementer`), with a $25 budget cap.
- **Workspace:** a separate git worktree. No human interaction during the run.
- **Prompt:** [`prompt.md`](prompt.md), now templated as `harness/prompts/single-agent-implementer.md` + `scope-m1.md`.
- **Agent's own report:** [`final-report.md`](final-report.md).

## Metrics

| Metric | Value |
| --- | --- |
| Outcome | 17/18 M1 checks passed. The 1 failure was caused by a factory bug (see F2). After v1.1 and the merge: 18/18. |
| Human interventions | 0 |
| Wall clock | 11.7 min (07:27–07:39 UTC) |
| Turns / cost | 60 turns, $3.64 |
| Output | 3 commits, 27 files, ~3,000 lines (including its own unit tests) |
| First-try pass rate | All SKEL, SEC and DEP checks passed on their first gate run |
| Coverage | api 99%, clock 100%, config 100%, store 89%, depcheck 96%, root package 75% |
| Escalations | 2, both real factory bugs |
| Hook denials | 4, all false positives (F1); no attempts to modify protected files |

## Findings (factory bugs) and fixes in v1.1

**F1: the shell-command hook gave false positives.**
- *What happened:* it blocked any write command that merely *mentioned* a protected path: a doc comment in a heredoc, a Makefile target that runs `harness/factory`, a commit message, or running the gate in the same command as a file edit.
- *Fix:* `harness/internal/policy/shell.go` now parses each command for its actual write targets and tracks `cd`. Heredoc bodies and quoted text count as content, not targets.
- *Verified:* replaying all 40 shell commands from the run blocks 0 (v1 blocked 4), and the block tests grew from 10 cases to 22.

**F2: next-milestone acceptance tests broke the current milestone.**
- *What happened:* the M2 `*_acceptance_test.go` files lived in product packages that don't exist at M1. So `go vet ./...`, staticcheck and coverage failed.
- *Why validation missed it:* the reference-implementation validation built M1 and M2 together.
- *Fix:*
  - Staged tests now live in `harness/_staged/m<N>/`, and `harness/factory promote --ms N` moves them in when the milestone starts.
  - Coverage floors are milestone-aware, as QUAL-01's text already said. The gate passes `GATE_MS_MAX`, and packages are mapped to milestones from the acceptance matrix.
  - The gate's "missing" detail points to the staged file.

**F3: the prompt contradicted itself.**
- *What happened:*
  - "the gate must exit 0" conflicted with "don't start M2";
  - the commit attribution line didn't match the environment's;
  - there was no guidance on writing files.
- *Fix:*
  - The rule "scope beats the definition of done" is now in the prompt and `CLAUDE.md`.
  - Commits use the attribution the environment gives.
  - Agents write files with Write/Edit and use Bash only for commands.
  - The prompt is now a template, plus a launcher (`harness/prompts/run-single-agent.sh`).

**F4: the spec was silent on three points, which the agent decided reasonably.** These are now adopted into `spec/contracts/go-api.md` and `spec/openapi.yaml`:
- An unsupported method on a known route returns 405 `invalid_request`.
- In `--dev` mode, non-API paths return 404.
- A `Host` header without a port means port 80.

## Observations

- **The agent read the harness and contracts first,** and then hit almost every check on the first try. Precise black-box tests plus pinned contracts left little room for interpretation.
- **Escalations were high quality.** Each was diagnosed correctly, came with options, and the agent's interim workaround didn't weaken anything: it used compiling stubs, not skipped checks. Both escalations pointed at the factory, not the product. That's the intended feedback loop.
- **The gate output gave good progress visibility** to an outside observer. Each run's `↳ M1 x/y` line was enough to follow along.
- **M1 is small and well specified,** so it isn't a strong test of Approach A. M2 (parsers plus a fuzz target, about 21 checks, with oracle-backed fixtures) and M4 (concurrency) will say more.

## Next

1. Promote the M2 acceptance tests and run Approach A on M2 for a second baseline: `harness/prompts/run-single-agent.sh 2`.
2. Then run Approach C (contract-first fan-out) on a comparable slice, and compare against this table.
