# Retro: Approach A (single agent + harness) on milestone 2

**Date:** 2026-11-07
**Branch:** `impl/m2-approach-a`, merged into `main`
**Factory version:** v1.1 (after the M1 retro fixes)

Same setup as [the M1 run](../2026-11-07-approach-a-m1/README.md). The prompt was rendered from `harness/prompts/single-agent-implementer.md` + `scope-m2.md`: [`prompt.md`](prompt.md). The agent's own report: [`final-report.md`](final-report.md).

## Metrics

| Metric | M1 run | **M2 run** |
| --- | --- | --- |
| Outcome | 17/18 (a factory bug caused the miss) | **39/39** (M1 still green) |
| Human interventions | 0 | **0** |
| Wall clock | 11.7 min | **10.7 min** (08:18–08:29 UTC) |
| Turns / cost | 60 / $3.64 | **41 / $2.34** |
| First full gate run | 10/10 of what it ran | **21/21 of M2** |
| Escalations | 2 (factory bugs) | **0** |
| Hook denials | 4 (false positives) | **0** |
| Coverage | ≥ 89% on new packages | bencode **97.5%**, metainfo **100%**, api 99.5% |
| Output | ~3,000 lines, 3 commits | ~1,500 lines of product code plus tests, 2 commits |

Independent checks after the run:
- `harness/factory gate --upto 2` passes 39/39.
- `factory integrity` passes, with 0 changes to protected files.
- `FuzzDecode` for 120 s (4× the gate's 30 s) found nothing.

## What worked

- **The v1.1 fixes held:** there were no hook false positives, no contradictions in the prompt, and the staged → promoted tests worked.
- **Oracle-backed black-box fixtures** (libtorrent + Transmission), plus pinned APIs, gave unambiguous targets. Every M2 check passed on the first gate run.
- **More scope than M1** (two parsers, a fuzz target, an HTTP endpoint) was **cheaper and faster**. The quality of the harness is what dominated, not the size of the task.

## Findings

**F5: interpretations made in agent notes, not in the spec.**
- *What happened:* where the spec was loose, the agent chose interpretations and recorded them in `PROGRESS.md`. Three of them are product behavior that a later milestone could contradict:
  - lenient mode also accepts leading zeros and `-0`;
  - a mistyped `announce`/`announce-list` is ignored instead of rejecting the torrent;
  - reserved names get `_` appended (`CON` → `CON_`).
- *Action:* the architect role should review `PROGRESS.md` interpretations at each milestone boundary, and either promote them to the spec or reject them. *Not done yet.*

**F6: the test was too easy for this approach.**
- *What happened:* M1 and M2 are pure, well-specified, mostly sequential work. That's where a single agent shines, so they don't stress Approach A.
- *Action:* M3 onwards (networking, concurrency, several packages that interact) is where approaches differ. M3 is run with Approach C for comparison.

## Next

The M3 harness: black-box download tests against a fake tracker, a fake peer, and a real libtorrent seeder. Then Approach C (contract-first fan-out) on M3.
