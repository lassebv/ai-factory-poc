# Retro: Approach C (contract-first fan-out) on milestone 3

**Date:** 2026-11-07/08
**Branch:** `impl/m3-c`, merged into `main` (`54f8821`)
**Factory:** `harness/prompts/run-fanout.sh` (architect → 4 parallel implementers → integrator)

The per-agent prompts and final reports are in the subdirectories, and the metrics are in [`summary.json`](summary.json). The comparison with Approach A is in [`../2026-11-08-approach-a-vs-c.md`](../2026-11-08-approach-a-vs-c.md).

## Metrics (the clean run)

| Agent | Turns | Cost | Wall clock |
| --- | --- | --- | --- |
| architect | 66 | $5.58 | 19 min |
| wp-peer | 20 | $1.11 | 6 min |
| wp-piece | 15 | $0.76 | 2 min |
| wp-storage | 14 | $0.81 | 3 min |
| wp-tracker | 16 | $0.89 | 3 min |
| integrator | 57 | $3.21 | 10 min |
| **Total** | **188** | **$12.36** | **≈35 min**: 19 + 6 (parallel phase) + 10; sum of agent time 43 min |

**Outcome:** 54/54 checks (M1 18, M2 21, M3 15). The integrator was green on the first full M3 gate run.

**Independent checks:**
- `go test -race` is clean.
- Coverage is 93–100% on every package.
- 120 s of fuzzing on `FuzzDecodeMessage` found nothing.
- The M3 download suite passed 3 out of 3 runs.
- The contracts were byte-for-byte unchanged after the architect.
- Integrity passes per role.

**Size:** about 8,300 lines added (product plus tests) in 35 files.

**Human interventions:** 0 inside the clean run. There were 3 operational restarts, none caused by the approach:
- the machine slept twice;
- a usage limit was hit once.

## What happened

- **The architect:** wrote 7 `contract.go`/`contract_test.go` pairs (tracker, peer, piece, storage, session, api, …), the plan (4 work packages plus integration) and ADRs 0012–0015. It also raised **two escalations before any implementation code existed**:
  1. The fake peer's `StallFrom` blocked its whole request queue. That made STOR-02 secretly depend on in-order piece selection, which contradicts §4.1's random-first. This was a harness bug; it's fixed now.
  2. The §6.2 shutdown order (HTTP drain first, up to 5 s) conflicts with TRK-01's 3-second budget for the `stopped` announce. It chose to run both concurrently (ADR-0015). **The human should confirm this and amend §6.2.**
- **The implementers:** each took 2–6 minutes and less than $1.20. All four met their contract tests with `-race` and 97–100% coverage. They wrote 0 escalations and triggered 0 hook blocks.
- **The integrator:**
  - **Merges:** 4 merges with no conflicts.
  - **Contracts:** it reported "no contract gaps, no misread contracts, no work-package bugs".
  - **Fixes:** the only breakage was an interplay with M2's preview upload reader, plus two mistakes in its own tests.
  - **Hazards outside the contracts:** it designed around two (event ordering on connect, and hashing only after a piece's writes finish). The architect's contracts should have covered these.

## Findings (factory bugs) and fixes

**F7: the machine sleeping kills runs.**
- *What happened:* two attempts died with "computer went to sleep mid-response". The first looked like API degradation (17 retries).
- *Fix:* launchers wrap agents in `caffeinate -ims`, and an `API Error` final message counts as a failed agent. (`d05c535`)

**F8: no way to resume.**
- *What happened:* a usage limit cut off phase 2, and the only option was to start again and pay for the architect phase twice.
- *Fix:* `run-fanout.sh --from-phase 2|3`. (`965e860`)

**F9: the integrity check didn't know about roles per commit range.**
- *What happened:* `integrity --base main --role implementer` blamed the architect's contract commits on the integrator.
- *Fix:*
  - `integrity --head` checks a commit range.
  - The launcher now checks the architect range (`main...ARCH_HEAD`, as architect) and the rest (`ARCH_HEAD...`, as implementer) separately.
  - The integrator's definition of done uses `ARCH_HEAD`.

**F10: the fake peer's stall serialized everything.** See escalation 1. *Fix:* only stalled requests are held, and there's a new unit test, `TestStallHoldsOnlyStalledPieces`.

**F11: the WIRE-04 text was out of date.** It said "200 peers" but the test uses 40. The text is fixed.

**F12 (open, human):** amend §6.2 to say the `stopped` announces start at the same time as the HTTP drain.

**F5 (still open, from M2):** promote or reject the interpretations recorded in `PROGRESS.md` at each milestone boundary.

## Watch watchers

The run watcher reported one stale "finished" event, from the transcript of the interrupted attempt, before the launcher replaced it. It's harmless, but when observing a run, check the `started_at` timestamps.
