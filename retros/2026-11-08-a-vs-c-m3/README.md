# Controlled comparison: Approach A vs Approach C on milestone 3

**Date:** 2026-11-08

**Setup:**
- **The same milestone, the same starting commit (`965e860`), the same harness and the same scope file.**
- **A:** one headless implementer session (`impl/m3-approach-a`).
- **C:** an architect, then 4 parallel implementers, then an integrator (`impl/m3-c`, merged to `main`).
- **Isolation:** A's transcript was audited, and A never read C's code.

**The question:** which approach gives the better implementation? Both pass the gate, so quality was measured four other ways:
1. **Hidden tests:** 17 scenarios that neither run saw.
2. **Static and dynamic metrics.**
3. **Blind review:** 4 reviewers, with the X/Y labels balanced.
4. **Experiments** that check each reviewer claim.

## Verdict

**C produced the better implementation, but not uniformly, and at 2.1× the cost.**

**Where C is better:**
- design, rated 8.0 vs 6.25 by all four reviewers;
- tests: 117 vs 46 test functions, rated 8.25 vs 6.0;
- robustness against a corrupting peer;
- spec-correct shutdown;
- memory use.

**Where A is better:**
- price, speed and simplicity;
- it avoids slow and stalled peers;
- CPU efficiency.

**Each approach shipped one serious robustness defect:**
- **A** can stall forever when a peer keeps sending corrupt data: H03 fails 3 out of 8.
- **C** hangs when a connected peer never answers requests for the last pieces: H14 fails 2 out of 2. This one is formally milestone 4 scope (endgame).

**In 3 of 4 blind reviews, the reviewer preferred to maintain C.**

## 1. Process

| | **A** | **C** |
| --- | --- | --- |
| Sessions | 1 | 6 (architect, 4 parallel implementers, integrator) |
| Turns | 91 | 188 |
| Cost | **$5.75** | $12.36 (architect $5.58, implementers $3.57, integrator $3.21) |
| Wall clock | **21 min** | ≈35 min (19 architect + 6 parallel + 10 integrator) |
| Gate | 54/54 | 54/54 |
| First full M3 gate run | 0/5 → 15/15 after iterating | 15/15 on the integrator's first run |
| Escalations | 1: the STOR-02 harness flaw, found during implementation | 3: the STOR-02 flaw *before any code*, the §6.2 conflict, the integrity base |
| Acceptance suite × 3 | 3/3 | 3/3 |

## 2. Hidden tests (`harness/acceptance/holdout/m3`)

Neither run saw these. Tests H01–H11 and P01–P02 were written **before** A finished. H12 was written after C's architect flagged its hazard, so it isn't blind to C. H13, H14 and P03 were written to test the reviewers' claims.

| Test | **A** | **C** |
| --- | --- | --- |
| H01 multiple peers, H02 peer drops, H04 tier failover, H05 failure reason, H06 choke mid-transfer, H07 `have` instead of bitfield, H08 bogus pieces, H09 SIGTERM mid-download, H10 dead peer, H13 poisoning peer | all pass | all pass |
| **H03 a peer that always corrupts, next to a good peer** | ❌ **5/8 pass**; failures stall at 96.9% forever; passes take 2–18 s | ✅ **8/8**, ~0.9 s |
| **H12 stopped announce while an HTTP request is in flight** (§6.2 + TRK-01 budget) | ❌ **5.00 s**: waits for the HTTP drain; will fail with any open connection, e.g. M6's SSE | ✅ 0.00 s |
| **H14 a connected peer stalls the last pieces forever** (endgame is M4 scope) | ✅ 1.5 s | ❌ **hangs, 2/2** |
| H11 30 slow peers: peers used | 20 | 30 |
| P01 256 MiB throughput | 607 MiB/s | 608 MiB/s |
| P01 CPU time for 256 MiB | **0.82–0.86 s** | 1.11–1.16 s |
| P01 peak RSS | 28 MiB | 29–30 MiB |
| **P02 one slow peer next to a fast one** | **0.21 s** (the slow peer is never used) | 1.63 s (5 blocks waited on the slow peer) |
| **P03 peak RSS, 8 MiB pieces from 16 peers** | 65–88 MiB (buffers whole pieces) | **28 MiB** (streams blocks to disk) |

**Root causes:**
- **A, H03:** after a failed hash, A's picker frees the piece, and whichever peer asks next gets it. The corrupt peer answers instantly, so it wins the retry again and again near the end of the download. Nothing remembers who sent the bad data.
- **A, H12:** A followed §6.2 literally, running the HTTP drain (up to 5 s) before the announces. C's architect spotted the conflict with the 3-second budget and changed the order (ADR-0015).
- **C, H14:** this follows directly from C's own frozen design (ADR-0012): no endgame, and a block requested from one peer is never handed to another. The contract made the hang structural.

## 3. Static and dynamic metrics (M3 packages plus API/cmd; `harness/prompts/quality-metrics.sh`)

| | **A** | **C** |
| --- | --- | --- |
| Product LOC | **3,480** | 4,626 |
| Test LOC | 1,775 | **4,139** |
| Test functions | 46 | **117** (includes the architect's contract tests) |
| Average coverage | 96.6% | 97.4% |
| Cyclomatic avg / max / functions over 15 | 4.98 / **31** / 9 | 4.98 / 45 / 13 |
| Channels / mutexes | 6 / 7 | **17 / 5** (closer to §4.5's actor model) |
| `ctx context.Context` parameters | 9 | **25** (§4.5: "all goroutines take a context") |
| Race detector | clean | clean |
| staticcheck `-checks all` | 1 | 1 |

## 4. Blind review (4 independent reviewers, 2 per label order; `reviews/`)

The first two reviewers each preferred whichever codebase was labelled X, which suggests position bias. Two more reviewers balanced the order. Mean scores out of 10:

| Dimension | **A** | **C** |
| --- | --- | --- |
| Correctness | 6.50 | 6.75 |
| Architecture / spec | 6.75 | 6.50 |
| **Design** | 6.25 | **8.00** (all four gave 8) |
| Readability | 7.75 | 7.75 |
| **Tests** | 6.00 | **8.25** (all four gave ≥ 8) |
| Performance | 6.25 | 6.00 |
| Security / bounds | 5.50 | 6.50 |
| **Total (out of 70)** | **45.0** | **49.75** |
| Prefer to maintain | 1 of 4 | **3 of 4**, including one with C in the Y position |

**Reviewer claims checked by experiment:**

| Claim | Raised by | Result |
| --- | --- | --- |
| A buffers whole pieces, so memory is unbounded | 4 of 4 | **Confirmed in direction** (P03: 65–88 vs 28 MiB) |
| C accepts unrequested blocks, so it can be poisoned | 3 of 4 | **Not reproduced** (H13 passes), but the code path is real |
| C has no endgame and no request timeout, so it hangs on a stalled peer | 2 of 4 | **Confirmed** (H14 hangs) |
| A: dial and peer goroutines aren't waited on at Close; no goroutine-leak test (§4.5) | 2 of 4 | Not tested here; C has a leak check in its tests |

**No reviewer found A's corrupt-peer stall (H03) or its shutdown-order bug (H12).** Only the hidden tests did. The reviews in turn found the memory and stalled-peer issues before any test covered them. **Reviews and adversarial tests catch different defects, so a factory needs both.**

## What this means for the factory

1. **What made C's code better was the architect.**
   - It wrote contracts and contract tests, which give the design and test-quality gap.
   - It reviewed risk up front, which caught §6.2 and STOR-02 before any code existed.
   - The parallel fan-out itself saved little time.

   The architect is also where C's main defect came from: ADR-0012 froze "no endgame" into the contract. **A frozen contract makes good and bad decisions equally hard to undo.**
2. **A is the efficient default, but it optimizes for passing the gate.** Its defects sit exactly where the gate is silent: adversarial peers, and shutdown under load.
3. **Recommendation for M4, the concurrency-heavy swarm engine: "A+".** That means an architect phase with contracts, contract tests and a risk review, followed by **one** implementer. It should keep most of C's quality gains at roughly A's cost. Compare it with this baseline.
4. **Harness improvements before M4:**
   - Promote H03, H12, H14 and P03 (with an RSS bound) into the M4 acceptance suite, so the next run must handle them whatever the approach. Keep a *fresh* holdout set for every milestone; once promoted, a holdout is no longer hidden.
   - Add a goroutine-leak check (§4.5) to the gate.
   - Make every milestone's harness include adversarial peers by default: corrupting, stalling, slow and poisoning.
5. **Use both reviews and tests as quality gates.** Blind multi-reviewer reviews, with balanced positions, plus experiments that check their claims, turned out cheap: about $8.50 for four reviews.

## Caveats

- **One run per approach.** Agent runs vary. A second A run might not have the H03 bug, and a second C architect might not freeze "no endgame".
- **H12 isn't blind to C** (see above). Without H12, the robustness score is 1–1, with A failing H03 and C failing H14.
- **The reviewers are the same model family as the builders,** so their stylistic preferences may correlate with C's more structured code.
- **C's test count includes the architect's contract tests.** That's fair, since they're part of the approach, but it isn't extra effort by C's implementers.
