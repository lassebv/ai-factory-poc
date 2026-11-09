# Controlled comparison: Approach A vs Approach A+ on milestone 4

**Date:** 2026-11-08/09

**Setup:**
- **Same base and harness:** both started from `f983f35`, with the same M4 harness (25 checks) and the same `scope-m4.md`.
- **A+:** an architect (contracts, contract tests, ADRs, risk review), then one implementer (`impl/m4-aplus`).
- **A:** one agent holding both the architect and implementer roles, so it could evolve the contracts too (`impl/m4-approach-a`).
- **Run order:** the runs were sequential, so timing-sensitive checks didn't interfere.
- **Isolation:** A's transcript was audited, and it never read A+'s code.

**The question:** does the architect phase pay off on a concurrency-heavy, multi-package milestone?

## Verdict

**Yes. A+ produced the clearly better implementation, at lower cost.**
- **Same gate result:** both pass the gate fully.
- **Hidden tests:** A+ passes all of them; A fails one, deterministically.
- **Seeding throughput:** A+ seeds about 2× faster at half the CPU.
- **Blind reviews:** **4 out of 4 preferred A+, in both label positions, 53.5 vs 43.0.**
- **Early risk discovery:** the architect found two spec gaps before any code existed.
- **Cost:** A+ cost $15.54 against A's $21.03, though part of A's cost came from resuming after an interruption.

## 1. Process

| | **A+** | **A** |
| --- | --- | --- |
| Sessions | architect + implementer | 1 (interrupted by a usage limit, then resumed) |
| Turns | 216 (84 + 132) | 142 (48 + 94) |
| Cost | **$15.54** ($6.11 + $9.43) | $21.03 ($4.84 + $16.19) |
| Active time | 57 min (21 + 36) | **52 min** (15 + 37) |
| Gate, with AC-06 re-scored to STOP-01 (F13) | 80/80 | 80/80 |
| First full M4 gate run | 22/26, one of them AC-06 | 12/13 of its first batch |
| Escalations | **5**: ban rule vs innocent peers, G18 gaps (both resolved in the contracts before any code existed), the AC-06 harness bug, a contract that contradicted its own test, plus the plan notes | 1 (AC-06) |
| Hook denials | 0 | 0 |

*Cost caveat:* A's second part resumed a large session after the usage limit, and reloading that context costs money. Without the interruption, A would probably have cost about the same as A+, not more.

## 2. Hidden tests (`harness/acceptance/holdout/m4`)

HM01–HM10 were written **before either run** and kept outside the repo until both finished.

| Test | **A+** | **A** |
| --- | --- | --- |
| HM01 10 leechers / HM02 torrentd→torrentd / HM03 swarm / HM04 restart while seeding / HM05 pause-restart-resume / HM06 stop-restart-resume / HM07 hostile mix / HM08 20 torrents | all pass, 2/2 | all pass, 2/2 |
| **HM09 `kill -9` while seeding** | ✅ 2/2 | ❌ **0/2**: after the restart it shows downloading at 0% even though the files are complete. The verified bitfield isn't persisted until the 10 s flush. That's within the letter of §11 ("loses at most 10 s"), but a fully seeded torrent would re-download everything. |
| **HM10 seeding throughput, 256 MiB** | **1,126–1,138 MiB/s, 0.55 s CPU** | 518–680 MiB/s, 0.9–1.1 s CPU |
| HM02 torrentd→torrentd throughput | 631–634 MiB/s | 628–629 MiB/s |
| HM11 pause/resume during the check (added after review) | 3/3 | 3/3 |

**Stability:**
- **RES-04:** 6/6 for both. A fixed the stalled-pipeline flake during its run; A+ had fixed the same root cause earlier with a periodic top-up.
- **Full M4 acceptance suite:** 2/2 for both.
- **M3 hidden tests:** both fail only H03. That one is obsolete at M4: H03 puts both peers on 127.0.0.1, and correct per-IP banning (G10) then bans the good peer too. BAN-01 covers the scenario properly.

## 3. Metrics (`harness/prompts/quality-metrics.sh`)

| | **A+** | **A** |
| --- | --- | --- |
| Product / test LOC | 8,098 / 5,401 | 6,899 / 4,811 |
| Test functions | 151 | 134 |
| Coverage | 95.2% | 95.9% |
| Cyclomatic avg / functions over 15 / max | 5.2 / 22 / 45 | 5.18 / 18 / 45 |
| `ctx context.Context` parameters | **66** | 45 |
| Separate `choke` package (§4.4) | yes | no (it's in the session) |
| Race detector / staticcheck | clean / 1 | clean / 1 |

## 4. Blind review (4 reviewers, 2 per label order; `reviews/`)

| Dimension | **A+** | **A** |
| --- | --- | --- |
| Correctness | 7.00 | 6.25 |
| **Architecture / spec** | **8.25** | 5.50 |
| **Design** | **8.00** | 6.25 |
| Readability | 7.00 | **7.50** |
| Tests | **7.75** | 6.25 |
| **Performance** | **7.75** | 5.50 |
| **Security / bounds** | **7.75** | 5.75 |
| **Total (out of 70)** | **53.5** | **43.0** |
| Preferred | **4 of 4**, in both positions | 0 |

**Why the reviewers preferred A+:**
- the choke package;
- worker pools at the storage level, process-wide (fixes an inherited defect);
- a picker that owns expiry and contributor tracking, using the injected clock;
- `context` on actions;
- bounded incoming handshakes;
- a goroutine-leak helper.

**High-severity defects claimed:** 1 against A+, 3 against A.

| Claim | Result |
| --- | --- |
| Pause/resume during the check races (claimed against A+, and separately against A) | **Not reproduced** (HM11 3/3 for both) |
| A: unbounded goroutines for incoming connections, so a flood can exhaust file descriptors | Not tested beyond FD-01's churn |
| A: pause doesn't cancel the hashing queued by a check | Not tested |

## 5. Harness findings from this round

- **F13: AC-06 was an unpassable placeholder.** It's fixed on `main`, mapped to the black-box STOP-01.
- **F14: RES-04 required robustness its text didn't state.** The text is clarified. Both runs fixed the real underlying problem: a stalled peer clogged the request pipeline.
- **Mutation tests on A+'s code:** 3 of 4 caught: unlimited upload slots → CHOKE-01; no banning → BAN-01; no restore at startup → 4 persistence checks fail. **"No endgame" was not caught**: request expiry also prevents the hang, so PICK-02 tests the outcome, not the mechanism.
- **Usage limits** interrupted 3 phases. `--from-phase` (for A+) and session resume (for A) recovered every time, without losing work.

## What this means for the factory

1. **The architect phase pays off on multi-package, concurrency-heavy work.** Across both comparisons, M3 (C vs A) and M4 (A+ vs A), the architected version won the blind reviews and the hardest hidden tests. On M4 the margin was larger, and A+ was also cheaper than C.
2. **The robustness risk review worked.** Unlike C's M3 architect, the A+ architect designed in request expiry, endgame, contributor tracking and exclusive retry. It also resolved two spec gaps up front.
3. **A remains the right choice for small, pure-logic milestones** (M1/M2-like): it's cheaper and simpler, with no quality difference.
4. **Recommendation:** use **A+ as the default for M5 (discovery) and M6 (API, limits, SSE)**, and plain A for narrow slices. Keep fresh hidden tests and balanced blind reviews for every milestone.
5. **Open items:**
   - promote HM09 (verified bitfield persisted on check completion) and the throughput floors into the M5 acceptance suite;
   - mark M3's H03 as superseded;
   - fold the architect's G18 and ban-rule clarifications into §16;
   - consider a direct endgame mechanism check (duplicate requests before any expiry).

## Caveats

- **One run per approach.**
- **A was interrupted and resumed;** A+'s architect lost only its final report.
- **The reviewers are the same model family as the builders.**
- **Two of the four high-severity claims weren't tested experimentally.**
