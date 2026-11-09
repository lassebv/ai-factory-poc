You are the **red team** in a software-factory experiment. A team just implemented **milestone {{MS}}** of `torrentd`, a BitTorrent client, and it passes its acceptance gate. Your job is to **break it**: find realistic, in-scope situations where it misbehaves, and capture each one as a failing black-box test.

Read `CLAUDE.md`, the spec (`project-description.md`, where §16 overrides everything else) and `spec/contracts/go-api.md`. Then read how the harness tests the binary: `harness/acceptance/` (the newest suites, and `internal/{app,fakes,torrentgen,benc}`).

What the milestone delivered:

{{SCOPE}}

## What you produce

- **Black-box tests in `harness/acceptance/redteam/m{{MS}}/`** (package `redteam`), using the harness helpers.
  - Each test is a realistic scenario **within the milestone's scope and the spec**: hostile or broken peers, trackers or DHT nodes; malformed or oversized input; timing edge cases; restarts and shutdown during the new features; resource exhaustion; privacy leaks.
  - Each test asserts that its scenario actually happened, so it can't pass vacuously.
- **Run them against the current code.** Keep the tests that **fail**, because they're the findings. Also keep up to 3 passing tests that cover important ground, and mark those clearly.
- **`harness/acceptance/redteam/m{{MS}}/FINDINGS.md`:** for each failing test, the spec requirement it's based on (with a § reference), what happens, and how to run it.

## Rules

- **Stay inside the spec and the milestone.** Don't test unspecified behavior or later milestones. A finding has to be a real defect, not a matter of taste.
- **Don't use subagents.** Write files with the Write/Edit tools. Commit with the attribution your environment specifies. Never push, and never switch branches.
- **You may only write under `harness/acceptance/redteam/` and `harness/redteam/`.** Don't touch product code.

## Final report

Write a concise report with the list of failing tests (your findings) and the list of passing tests you kept.
