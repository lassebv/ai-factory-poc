# Retrospective: Approach A vs Approach C (milestones 1–3)

> **Update (2026-11-08):** the controlled comparison recommended below has been run. See [`2026-11-08-a-vs-c-m3/`](2026-11-08-a-vs-c-m3/README.md). Result: on the same milestone and harness, **C produced the better implementation** (design, tests, robustness against corrupt peers, shutdown correctness, memory) at **2.1× A's cost**, and each approach shipped one serious robustness defect. The recommendation for M4 is unchanged: **A+**, an architect phase followed by one implementer.

**Question:** which factory approach has worked best so far: **A** (one implementer agent with a strong harness) or **C** (an architect freezes contracts, parallel package implementers build against them, an integrator merges)?

**Short answer:**
- **A has been the most efficient for small, well-specified milestones.**
- **C delivered the highest-quality result on the first multi-package milestone**, with zero integration friction. It cost about 3–5× more per milestone, and its parallel speed-up was small at this size.
- **The comparison isn't controlled yet:** A ran on M1 and M2, and C ran on M3, a harder milestone. One more run settles it (see the end).

## The data

| | **A on M1** (Skeleton) | **A on M2** (Bencode + metainfo) | **C on M3** (Single-peer download) |
| --- | --- | --- | --- |
| New checks | 18 | 21 | 15 (heavier: networking, interop) |
| Result | 17/18; the miss was a factory bug, 18/18 after the fix | **39/39** | **54/54** |
| First full gate run | 10/10 of what it ran | 21/21 | 15/15 (integrator) |
| Wall clock | 11.7 min | 10.7 min | ≈35 min (19 architect + 6 parallel + 10 integrator) |
| Agent time (sum) | 11.7 min | 10.7 min | 43 min |
| Turns | 60 | 41 | 188 |
| Cost | $3.64 | $2.34 | **$12.36** |
| Size added | ≈3,000 lines | ≈2,500 lines | ≈8,300 lines |
| Cost per 1k lines | ≈$1.2 | ≈$0.9 | ≈$1.5 |
| Coverage (new packages) | 89–100% | 97–100% | 93–100% |
| Escalations | 2 (both factory bugs) | 0 | 3: 1 spec contradiction, 2 factory/harness bugs |
| Hook false positives | 4 (fixed in v1.1) | 0 | 0 |
| Human interventions in-run | 0 | 0 | 0 |
| Operational restarts | 0 | 0 | 3 (sleep ×2, usage limit). Not caused by the approach, but C's 6 sessions are more exposed. |

## What each approach is good at

**Approach A strengths:**
- **Cheapest and simplest:** one session, one prompt, nothing to orchestrate. On M1 and M2 it was green almost immediately, because the harness and pinned contracts left little room for interpretation.
- **Its quality was not worse:** coverage, first-try pass rates and escalation quality were all excellent.
- **It's robust to interruptions:** a single session is easy to rerun.

**Approach A weaknesses:**
- **Interpretations end up in the agent's notes, not in contracts (F5).** On M2 the agent made sensible but unreviewed product decisions in `PROGRESS.md`.
- **Nothing forces it to design up front.** On M1 and M2 that didn't matter. On concurrency-heavy milestones it's a risk.

**Approach C strengths:**
- **The architect found two real problems before any implementation code existed:**
  - a harness design flaw (the fake peer's stall made STOR-02 depend on piece order);
  - a spec contradiction (the §6.2 shutdown order vs the TRK-01 budget).

  Approach A would most likely have run into these later, or worked around them silently.
- **Frozen contracts held perfectly:**
  - 4 independent agents with no merge conflicts and no contract gaps;
  - contracts byte-for-byte unchanged;
  - the integrator green on its first gate run.
- **Decisions are recorded where they belong:** ADRs 0012–0015 and `contract.go`, not in notes.
- **Work packages are cheap and fast:** 2–6 minutes, less than $1.20 each, 97–100% coverage.

**Approach C weaknesses:**
- **The architect phase dominates:** 45% of the cost and 54% of the wall clock, and it runs serially. Parallelism saved only about 8 minutes here, because the four packages were small. **C's speed advantage only appears when the parallel work is much bigger than the design work.**
- **More factory complexity means more factory bugs:** the integrity check didn't understand per-role commit ranges (F9), resume had to be added (F8), and there are more sessions that can be interrupted.
- **Hazards that cross package boundaries still land on the integrator.** It found two (event ordering on connect, hash-after-write ordering) that the contracts didn't cover.

## Verdict

**For torrentd so far, A is the better default, and C is better where the design risk is high.**

1. **Use A for milestones that are mostly sequential, single-package or pure logic.** Examples: M2-style parsers, the M7 UI, M6 API endpoints on top of finished engines.
2. **Use C (or a hybrid) for milestones where several packages interact under concurrency.** Example: M4, the swarm engine (picker, choker, peer manager, persistence, fast resume). There, frozen contracts and an up-front design review are worth the extra ~$8.
3. **Try a hybrid, "A+": an architect phase followed by a single implementer.** This keeps C's main proven benefit (contracts, ADRs and early risk discovery) without the orchestration overhead. On M3 the parallel phase was cheap anyway. What made C good was the architect, not the fan-out.

## How to make the comparison fair (recommended next step)

Run **A on M3** against the same harness, from the pre-M3 `main` (`965e860`, plus the v1.1 fixes), in a throwaway branch. It should cost about $5–10. That gives a same-milestone comparison of cost, time, first-try pass rate and the quality of the decisions. In particular: does A trip over the §6.2 shutdown-order conflict that the architect caught?

Then run **A+ on M4**, the first truly concurrency-heavy milestone, and compare it with C on M4 if the budget allows.

## Factory lessons, whatever the approach

- **Harness quality decides everything.** Every milestone with a validated, oracle-backed, black-box harness went green on the first or second gate run. All the problems found were factory bugs, not product bugs.
- **Validate the harness against a reference implementation, and run mutation tests,** before every milestone. It caught 4 harness bugs before any factory run. The ones it missed (F2, F10) were interactions between milestones and between the test and the client's ordering, so validation should also include a "current milestone only" run and a spec-compliant random ordering.
- **Escalations work.** All 5 escalations across the runs were correct, diagnosed, came with options, and were handled without weakening anything. They're the main signal for improving the factory.
- **Operational robustness matters as much as prompts:** sleep, usage limits and resume. It's worth treating as a first-class part of the factory.
