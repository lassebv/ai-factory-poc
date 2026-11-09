# Building `torrentd` with a Software Factory: Approaches to Try

This document suggests ways to build the `torrentd` project (see `project-description.md`) with a multi-agent "software factory". The real goal is not the torrent client. The goal is to **learn how to design, run and improve a factory**, and `torrentd` is the workload you test it on.

---

## 1. What a "software factory" means

You listed three ideas: intent and guidelines, collaborating agents with roles, and a harness instead of reviewing code yourself. All three are right. They do not matter equally, though, and the order is worth correcting:

| Your point | How much it matters | Why |
| --- | --- | --- |
| **Harness that validates the result** | **Most** | It is the only thing that lets you stop reading code. Without it, agents produce plausible code and you are back to reviewing it line by line. |
| **High-level intent and guidelines** | High | The spec and conventions are the factory's input. If the spec is vague, the output varies from run to run. |
| **Agents in roles, working in parallel** | Medium | Parallelism speeds things up, but it only works once you have **contracts** (interfaces and tests) that let independent work fit together. Roles like "PM" or "architect" help only when each has a distinct input, output and acceptance check. Without those, they are just theatre. |

A useful way to sum it up:

> **Prompt-based development:** human → prompt → code → human reviews code → repeat.
> **Factory development:** human → *spec + harness + process* → agents produce code → **harness** judges → human reviews *the harness, the spec, and the metrics*, and improves the factory.

Two consequences of this shift:

1. **The code is disposable. The factory is the asset.** If the output is wrong, you fix the spec, the harness or the process, then re-run. Hand-patching the code just hides the factory's defect. A good test of your factory: delete `internal/` and re-run it. Do you get a working system again?
2. **You do your engineering work earlier.** Your judgment goes into decomposition, interface contracts, acceptance criteria, test oracles and escalation rules. Line-by-line review is no longer where it goes.

---

## 2. Why `torrentd` is a good testbed

Its strengths:

- **External oracles exist.** qBittorrent, Transmission and public trackers are ground truth that agents cannot "game". If a real Transmission instance downloads your seed, the code works.
- **The protocol is fully specified.** The BEPs are precise, public and in the models' training data.
- **Hard constraints are machine-checkable.** You get `depcheck`, `CGO_ENABLED=0`, binary size, and the performance and RSS targets.
- **It splits naturally into modules.** There are about 11 engine packages, plus store, API and frontend. That is real room for parallel work.
- **It contains a mix of difficulty.** Some parts are easy and mechanical (bencode, format.ts), some are subtle (choker, piece picker, endgame), and some are concurrency-heavy (session actor model, §4.5). You will see where agents struggle.
- **It contains a mix of work types.** Backend, frontend, DB, E2E and performance work exercise different agent skills.

Its risks:

- **It is big.** It is realistically tens of thousands of lines. Don't start with the whole thing (see §6).
- **Some oracles are expensive.** Docker interop and a real-network DHT are slow and flaky. You need a tiered harness so fast feedback dominates.
- **The spec has gaps.** This is useful, because it tests how your factory handles ambiguity (see §5.1).

---

## 3. Building blocks every approach needs

The approaches in §4 combine these components differently, but every one of them needs all of these.

### 3.1 Spec → executable acceptance criteria
Turn `project-description.md` into a machine-readable list of requirements, each linked to a check:

```yaml
# spec/acceptance.yaml
- id: BENC-03
  source: "§4.1 Bencode"
  text: "Rejects non-canonical integers (i03e, i-0e)"
  check: "go test ./internal/bt/bencode -run TestRejectNonCanonical"
  tier: L1
- id: AC-06
  source: "§14.6"
  text: "Stop seeding → no further upload, stopped announce sent, files kept"
  check: "go test ./tools/swarm -run TestStopSeeding"
  tier: L4
```

This traceability matrix is your **progress bar** and your **definition of done**. "Percent of requirements green" is the factory's main output metric.

### 3.2 A tiered verification harness

| Tier | What it checks | Typical runtime | Example for `torrentd` |
| --- | --- | --- | --- |
| L0 Static | Build, vet, gofmt, lint, depcheck, cross-compile, binary size | seconds | `CGO_ENABLED=0 GOOS=windows go build`, `tools/depcheck` |
| L1 Unit | Per-package behavior against BEP test vectors | seconds | Known info-hashes of public torrents, magnet base32 vectors |
| L2 Fuzz | Parsers never panic, round-trip stability | 30 s per target on each PR, longer nightly | Bencode decoder, wire message decoder |
| L3 Component | Package against fakes | seconds–minutes | `httptest` tracker, UDP tracker with drops, 50-node loopback DHT |
| L4 Swarm | In-process tracker + seeder + leechers | minutes | Completion, hash correctness, stop seeding, ratio limit |
| L5 Interop | Real third-party clients | minutes | Docker qBittorrent-nox and Transmission, both directions |
| L6 E2E | UI acceptance via Playwright | minutes | Add → Seeding → Stop seeding → Restart → state persisted |
| L7 NFR | Performance and resource budgets | minutes | ≥ 50 MB/s loopback, RSS < 200 MB at 200 torrents, SSE < 100 KB/s |
| Cross-cutting | Race detector, goroutine leak checker | — | `go test -race`, the in-repo goleak helper |

Agents iterate locally on L0–L3. The merge gate runs L0–L4. L5–L7 run nightly or at milestones.

### 3.3 Anti-gaming measures (critical)
Agents that are told to "make the tests pass" will sometimes weaken the tests. Defend against this structurally:

- **Separate ownership.** Tests and fixtures in `harness/` (or `*_acceptance_test.go`) belong to the verifier role. Implementer agents **cannot edit them**. Enforce this with tool permissions or a `PreToolUse` hook that blocks writes to those paths. An instruction in the prompt is not enough.
- **Holdout tests.** Keep a set of acceptance tests that implementers never see, and run them only at the gate. This catches overfitting.
- **External oracles.** Interop with real clients and known info-hashes of real torrents are hard to fake.
- **Mutation testing.** Occasionally inject bugs and check that the harness catches them. This measures the harness, not the code.

### 3.4 Contracts that make parallel work possible
Freeze before fan-out:

- Go interfaces at package boundaries (e.g. `piece.Picker`, `storage.Storage`, `tracker.Announcer`, the `session` ↔ `stats` counter API).
- Event types between peer goroutines and the torrent actor (§4.5).
- The OpenAPI/JSON schema for §8, which the backend and frontend develop against independently.
- The DB schema (§7.1, already provided).

Contracts should change through an explicit "change request" to the architect role, never silently.

### 3.5 Isolation and integration
- One **git worktree or branch per work unit**, so agents don't step on each other.
- A **merge queue**: rebase → full L0–L4 gate → merge. Merge one at a time so main is always green.
- A **status board** (issues, a `backlog/` folder of markdown tickets, or a small SQLite table) as the single source of truth for who is doing what.

### 3.6 Factory observability
Record every run: tickets, attempts per ticket, gate failures by tier, tokens/cost, wall-clock time, and human interventions (with the reason). Without this you cannot compare approaches or tell whether a change helped.

---

## 4. Approaches

Each approach below is a factory design you can try. They are not mutually exclusive. §6 suggests an order for experimenting with them.

### Approach A — "Strong harness, single agent" (baseline / control group)

**Shape:** One long-running agent, given the spec, `acceptance.yaml` and the harness, works milestone by milestone until the gate is green.

```
spec + harness ──▶ [ single agent loop: plan → code → run gate → fix ] ──▶ green?
```

- **Why try it:** It tells you how much of the value comes from the harness alone, not from multi-agent orchestration. It is often surprisingly strong. Without a baseline, you can't tell whether the extra complexity pays off.
- **Strengths:** Simple. No integration overhead. Consistent style.
- **Weaknesses:** Serial, so wall-clock time is slow. Its context fills up on a large codebase, so it needs good notes and memory files (`PROGRESS.md`, ADRs).
- **Human role:** Write the harness and spec. Intervene only when it is stuck.

### Approach B — "Assembly line" (role-based pipeline)

**Shape:** Each work unit passes through specialized stages. Each stage has its own agent definition, tools and output artifact.

```
Spec auditor ─▶ Architect ─▶ Test author ─▶ Implementer ─▶ Reviewer ─▶ Integrator
 (ambiguity     (package     (failing        (makes tests   (rubric:     (merge queue,
  report, Qs)    contracts,   tests from      pass, can't    spec, §4.5   full gate)
                 ADRs)        BEP + spec)     edit tests)    concurrency,
                                                             security)
```

- **Why try it:** It matches the "roles" picture most people have, and it makes each role's value measurable. For example: what share of defects did the reviewer catch before the gate did?
- **Make the roles real:** Give each role a **distinct artifact** and a **narrow tool set**. The test author writes only `_test.go` files. The reviewer is read-only and returns a structured verdict. The architect writes only interfaces and ADRs.
- **Strengths:** Clear separation of concerns. Writing tests first is built in. Easy to find which stage is weak.
- **Weaknesses:** Handoffs lose context. It can be slow if the stages run serially per ticket. You get parallelism *across* tickets, not within one.
- **Experiment:** Remove one role at a time (e.g. the reviewer) and check whether the escaped-defect rate changes.

### Approach C — "Contract-first fan-out" (parallel by package)

**Shape:** An architecture phase freezes the contracts. Then N agents implement packages in parallel, each in its own worktree, against fakes or mocks of their neighbors. An integration phase follows.

```
                       ┌─▶ bencode+metainfo agent ─┐
                       ├─▶ tracker (http/udp) agent ┤
Architect freezes  ────┼─▶ peer wire agent ─────────┼─▶ Integrator: session wiring,
interfaces + fakes     ├─▶ piece picker agent ──────┤   swarm test L4, interop L5
                       ├─▶ storage agent ───────────┤
                       ├─▶ dht agent ───────────────┤
                       ├─▶ store + API agent ───────┤
                       └─▶ frontend agent (vs. ─────┘
                           OpenAPI mock server)
```

- **Why try it:** This is where parallelism actually pays off. It also tests how good your **contracts** are. Most of the pain shows up at integration, and that tells you what to specify better next time.
- **Fit for `torrentd`:** Very good. §4.4 already gives the package layout, and the dependency graph is mostly a tree (bencode → metainfo → tracker/peer → session).
- **Strengths:** Fastest wall-clock time. Each agent has a small, focused context.
- **Weaknesses:** Integration hell if the contracts are underspecified. This hits hardest at `session`, the orchestrator where concurrency bugs live. Agents may each solve shared problems (rate limiting, logging, error types) in their own way.
- **Mitigations:** Make a "shared kernel" package, written first, that holds common types and errors. Add contract tests that every implementation of an interface must pass. Have the integrator own `session` and nothing else.

### Approach A+ — "Architect first, then a single implementer"

**Shape:** Approach C's architect phase, followed by Approach A's single implementer. An architect freezes the contracts (`contract.go`, `contract_test.go`), records the design decisions as ADRs, and reviews the milestone for risks in the spec and harness. Then **one** implementer builds every package and the integration against those contracts.

```
spec + harness ──▶ Architect ──────────────────────▶ Single implementer ──▶ gate
                   (contracts, contract tests,        (all packages +
                    ADRs, risk review, escalations)    integration, can't edit
                                                       contract files)
```

- **Why try it:** In the controlled M3 comparison ([`retros/2026-11-08-a-vs-c-m3/`](retros/2026-11-08-a-vs-c-m3/README.md)), C's quality lead came from the **architect**, not from the parallel fan-out:
  - contracts and contract tests explained the design and test-quality gap (blind reviews rated design 8.0 vs 6.25 and tests 8.25 vs 6.0);
  - the up-front risk review caught a spec contradiction and a harness flaw before any code existed.

  The fan-out saved little time on M3, and all the extra sessions added orchestration overhead and more ways to fail. A+ tests whether the architect alone captures C's quality at roughly A's cost.
- **Fit for `torrentd`:** Good for multi-package, concurrency-heavy milestones where the design risk is high but each package is small. M4 (the swarm engine) and M5 (discovery) are examples. For small, pure-logic milestones, plain A is cheaper.
- **Strengths:**
  - Keeps the frozen contracts and contract tests, so the design is explicit and the tests are strong.
  - Decisions are recorded as ADRs, and risks are found early.
  - There's no integration phase and no merges between agents.
  - There are only 2 sessions, so cost and operational exposure stay close to A.
- **Weaknesses:**
  - Wall-clock time is serial: architect, then implementer.
  - The implementer's context has to hold every package.
  - Frozen contracts make bad decisions as sticky as good ones. On M3, C's ADR-0012 froze "no endgame", which made the H14 hang structural.
- **Mitigations:**
  - The architect's risk review should explicitly consider robustness scenarios: corrupt, stalled and slow peers, and shutdown under load.
  - Let the implementer propose a contract change through an escalation, with a fast architect turnaround, instead of working around a bad contract.
  - Run fresh holdout tests and a blind review at the end, as in the M3 comparison.
- **Experiment:** Run A+ on M4, then compare cost, time, hidden-test results and blind-review scores against the M3 baselines for A and C.
- **How to run:** `harness/prompts/run-fanout.sh` covers the architect phase. A single-implementer phase that reads the architect's plan is still to be added (`run-aplus.sh`).

### Approach A+R — "A+ with a review loop"

**Shape:** A+, plus a quality loop inside the factory. After the implementer finishes, **2 blind reviewers** (using the rubric from the comparisons) and a **red-team agent** examine the result. The red team writes adversarial tests in `harness/redteam/`. Their findings go back to the implementer for **one fix round**, and then the gate runs again.

```
Architect ─▶ Implementer ─▶ ┌ Reviewer 1 ┐
                            ├ Reviewer 2 ┼─▶ findings ─▶ Implementer (fix round) ─▶ gate
                            └ Red team   ┘
```

- **Why try it:** in the M3 and M4 comparisons, every real quality difference was found **outside** the factory, by holdouts and blind reviews that a human ran afterwards. The reviews were cheap (about $2–3 each) and found real defects. This puts that step inside the factory.
- **Strengths:**
  - Quality issues the gate is silent on get fixed before merge.
  - Red-team tests grow the harness, as a ratchet.
  - The review cost is small compared with the cost of implementation.
- **Weaknesses:**
  - The reviewers are the same model family as the builders, so they share blind spots.
  - Unverified review claims can send the fix round down false paths: in the comparisons, half the claims weren't reproducible.
- **Mitigations:**
  - The fix round must reproduce a finding, with a test, before fixing it. Findings that can't be reproduced get listed, not fixed.
  - A fresh human-run holdout set stays the final judge.
- **Experiment:** M5 (with a Sonnet implementer, see A+/S). Compare holdout results and review scores before and after the fix round.

### Approach A+/S — "A+ with a cheaper implementer"

**Shape:** A+, but the architect runs on the strongest model (Opus) and the implementer on a cheaper one (Sonnet).

- **Why try it:** the M4 comparison suggested that A+'s quality came mostly from the architect's contracts and risk review. If that's true, a cheaper implementer should keep most of the quality at a lower cost. At the scale of an organization, the cost per milestone matters.
- **Strengths:** lower cost and probably faster implementation. It also isolates how much the design contributes compared with the implementer model.
- **Weaknesses:** a weaker implementer may misread contracts, take shortcuts toward the gate, or struggle with concurrency bugs that the gate doesn't show.
- **Experiment:** M5 with A+R. Judge the result by the gate, fresh holdouts and blind reviews against the Opus-A+ M4 baseline. For a clean comparison, optionally run an Opus implementer on the **same** architect output.

### Approach V — "Agent-built harness"

**Shape:** a **verifier agent** writes the milestone's harness from the spec: acceptance entries, black-box tests, fakes and oracle scripts. It validates the harness itself: fakes against libtorrent, every check failing for the right reason on the current code, and mutation tests where possible. The human reviews the result instead of writing it.

- **Why try it:** in M1–M4, building and validating the harness was the biggest share of human effort, and the most important one. If the harness can be delegated, the factory scales. If it can't, the human role is "harness author" rather than "reviewer".
- **Strengths:** frees the scarcest resource, which is senior attention on the harness.
- **Weaknesses:**
  - A flawed harness misleads every run that follows.
  - Agents may write tests that are easy to pass, or that mirror the code they expect.
- **Mitigations:**
  - The human writes a small, independent control set of checks, kept hidden from the verifier.
  - Run mutation tests on the verifier's harness.
  - Cross-check against the control set after the run.
- **Experiment:** the M5 (discovery) harness. Compare it with the human control set: what does each catch that the other misses?

### Approach D — "Adversarial / red-team harness"

**Shape:** Builders on one side. On the other side are attacker agents whose only job is to make the system fail. They write malformed-peer fakes, hostile trackers, path-traversal torrents, DHT floods and kill-9 tests.

```
Builder agents ──▶ code ◀── Red-team agents (fuzzers, malicious peers, chaos)
                     │              │
                     └── failures become new harness tests (ratchet) ──┘
```

- **Why try it:** It grows the harness automatically, and it targets §11 (robustness, security), where happy-path tests are weakest.
- **Key rule:** Every failure a red-team agent finds becomes a **permanent regression test** in `harness/`. The harness only ever gets stronger. Think of it as a ratchet.
- **Pairs well with:** B or C, as an additional stage or a background loop.

### Approach E — "Orchestrator + backlog" (kanban factory)

**Shape:** A lead or orchestrator agent owns a backlog derived from `acceptance.yaml` and the dependency graph. Worker agents pull tickets that are ready (all dependencies done), work in worktrees, and submit to a merge queue. The lead reprioritizes on failures and escalates to the human when it is blocked.

```
          ┌──────────── backlog (tickets w/ deps, acceptance IDs) ───────────┐
          ▼                                                                  │
   Orchestrator ──assigns──▶ Worker 1..N (worktrees) ──PR──▶ Merge queue + gate
          ▲                                                       │
          └─── results, failures, metrics ◀───────────────────────┘
                     │
                     └──▶ Human: escalations only (ambiguity, contract change, stuck > N tries)
```

- **Why try it:** It is the most "factory-like" design: long-running, self-scheduling, with throughput limited only by the number of workers and how fast the gate runs. It is closest to what an organization would run in production.
- **Strengths:** It scales. Its escalation policy is explicit, which is your main lever for reducing human involvement.
- **Weaknesses:** Building the orchestrator itself is a project. Without good observability it is hard to debug.
- **Implementation options:** Claude Code subagents plus a backlog folder (cheapest). Headless `claude -p` workers driven by a script or Makefile. The Claude Agent SDK, for a custom orchestrator with full control over retries, budgets and logging.

### Approach F — "Best-of-N / tournament" for hard modules

**Shape:** For the hardest units (piece picker with endgame, choker, DHT routing table, the session actor), spawn N independent implementations. Run all of them through the harness, plus benchmarks and the review rubric, and keep the best one.

- **Why try it:** Agent output varies a lot from attempt to attempt. When the harness is cheap to run and the unit is self-contained, parallel attempts are cheaper than serial debugging.
- **Selection criteria:** Gate pass rate, then holdout tests, then performance (L7), then a simplicity score from a reviewer agent.
- **Use sparingly:** It multiplies cost. It is only worth it where the harness can really tell the candidates apart.

### Approach G — "Milestone-gated with human checkpoints"

**Shape:** Follow the spec's §15 milestones. Any approach above can run inside a milestone. At each milestone boundary, the human reviews **the harness results, a demo, the metrics and any changed contracts or ADRs**, not the code.

- **Why try it:** It is the realistic adoption path for a large organization. It keeps a human in control of direction while handing execution to agents.
- **Checkpoint checklist:** Did the acceptance IDs for this milestone go green? Were any harness files changed, and by whom? What did agents escalate? What did they decide on their own that should have been escalated?

### Comparison

| Approach | Parallelism | Up-front harness effort | Orchestration complexity | Human involvement | Best for learning… |
| --- | --- | --- | --- | --- | --- |
| A Single agent | Low | High | Low | Low–medium | How far the harness alone gets you |
| A+ Architect + single agent | Low | High (contracts too) | Low–medium | Low | Whether up-front contracts and risk review capture C's quality without the fan-out |
| A+R A+ with a review loop | Low | High | Medium | Low | Whether in-factory reviews and a red team fix what only post-hoc evaluation found |
| A+/S Cheaper implementer | Low | High | Low–medium | Low | How much quality comes from the design vs the implementer model; cost at scale |
| V Agent-built harness | — | Delegated | Medium | Review only | Whether harness authorship, the key human task, can be delegated |
| B Assembly line | Medium | High | Medium | Low | Which roles add value |
| C Contract fan-out | High | High (contracts too) | Medium | Medium at integration | How good your decomposition and contracts are |
| D Red team | Medium | Grows the harness itself | Low–medium | Low | Robustness and harness growth |
| E Orchestrator | High | High | High | Lowest (escalations only) | End-to-end autonomy and escalation design |
| F Best-of-N | High (per unit) | Needs strong tests | Low | Low | Cost vs. quality trade-offs |
| G Milestone-gated | Any | Any | Any | Periodic | Governance and adoption model |

---

## 5. Things specific to this project to set up first

### 5.1 Run a "spec auditor" pass before anything else
Let an agent (or several) read the spec adversarially and produce a list of questions and decisions. A few gaps I noticed while reading it:

- **There is no "retry" endpoint.** §5.3 says `error ──(user "retry")──▶ checking`, but §8.1 has no endpoint for it.
- **The half-open limit has no setting.** §9.6 lists a "half-open limit" setting, and §4.1 has a default of 20, but `max_half_open` is missing from the default settings in §7.2.
- **The ratio can divide by zero.** `uploadedTotal / max(downloadedTotal, completedBytes)` is undefined for a fresh magnet torrent where both are 0.
- **Port precedence is unclear.** If `--peer-port` is set and the user then changes `listen_port` in the UI, which one wins?
- **Some stats are undefined before metadata arrives.** `seedsConnected` (a peer "that has every piece") has no meaning before the piece count is known.

Each answer becomes a short ADR in `spec/decisions/`. **How your factory handles ambiguity is one of the most important things to learn.** Does it escalate, guess consistently, or guess differently in different places?

### 5.2 Write the high-value oracles by hand (or review them carefully)
This is where your senior-engineer time pays off most:
- Fixture `.torrent` files with **known info-hashes** (e.g. public Linux distro torrents).
- A Docker Compose interop setup with qBittorrent-nox and Transmission (L5).
- A `tools/swarm` scenario DSL, so acceptance scenarios read like the spec: "seeder S, leechers L1–L3, ratio limit 1.0 → expect S stopped with reason `ratio_limit`".
- The goroutine-leak helper and race-detector settings.

### 5.3 Conventions file (`CLAUDE.md` / `AGENTS.md`)
Keep it short and enforceable: package layout (§4.4), the concurrency rules (§4.5, i.e. "peer goroutines never touch `Torrent` state"), error handling, logging (`slog`), no new dependencies without passing depcheck, and the commands for each harness tier. Where possible, back each rule with a check (lint rule, depcheck, a custom `go vet` analyzer) rather than prose alone.

### 5.4 Suggested repo layout for the factory itself

```
.
├── project-description.md        # intent (human-owned)
├── spec/
│   ├── acceptance.yaml           # requirement → check traceability
│   └── decisions/                # ADRs from the spec audit and contract changes
├── harness/                      # verifier-owned; read-only for implementers
│   ├── fixtures/                 # .torrent files, known hashes
│   ├── holdout/                  # hidden acceptance tests (gate only)
│   ├── interop/                  # docker-compose for qBittorrent/Transmission
│   └── gate.sh                   # runs tiers L0..Ln, emits JSON results
├── .claude/
│   ├── agents/                   # role definitions (architect, test-author, implementer, reviewer, red-team…)
│   ├── settings.json             # permissions + hooks (e.g. block edits to harness/)
│   └── skills/                   # repeatable procedures ("add a BEP test vector", "run gate")
├── backlog/                      # tickets (if using Approach E)
├── runs/                         # per-run metrics/logs for comparing factory versions
└── (product code: cmd/, internal/, web/, tools/ …)
```

---

## 6. Recommended iteration plan

Treat this as an **experiment on the factory**. Change one variable at a time, and measure.

### Phase 0 — Harness and contracts (mostly human + one agent)
- Spec audit → ADRs.
- `acceptance.yaml` for **milestones 1–2 only** (skeleton, bencode, metainfo, `/torrents/preview`).
- L0–L2 harness, fixtures, the gate script, and permission hooks that protect `harness/`.
- **Time box:** this is where most of your personal effort should go.

### Phase 1 — Baseline on a small slice
Run **Approach A** on milestones 1–2. Record the metrics. Then delete the code and run **Approach B or C** on the same slice. Compare.
*Small, highly testable slices (bencode is ideal) tell you a lot about the factory cheaply.*

### Phase 2 — Scale to the engine core
Milestones 3–4 (single-peer download, then the swarm engine). Use **Approach C** (fan-out by package) with the L4 swarm harness as the integration gate. Add **Approach F** for the piece picker and choker. Expect the session/concurrency integration to be the hard part. That is the point of this phase.

*Status (2026-11-08):* M3 was run with both A and C ([comparison](retros/2026-11-08-a-vs-c-m3/README.md)). C produced better quality at 2.1× A's cost, and that quality came from the architect. M4 will therefore be run with **A+**.

*Status (2026-11-09):* M4 was run with A+ and A ([comparison](retros/2026-11-09-a-vs-aplus-m4/README.md)). A+ won clearly: blind reviews 53.5 vs 43.0 (4 of 4 preferred it), it passed all hidden tests (A failed one), it seeds 2× faster, and it was cheaper. A+ is now the default for multi-package milestones.

### Phase 3 — Autonomy
Milestones 5–7 (discovery, API, UI). Move to **Approach E** (orchestrator + backlog). Add **Approach D** (red team) as a background loop against parsers and peers. Frontend and backend develop in parallel against the OpenAPI contract.

### Phase 4 — Hardening and NFRs
Milestone 8. Interop (L5), E2E (L6), performance (L7). This shows whether the factory can work toward **measurable non-functional targets**, which is a different skill from making tests pass.

### Phase 5 — Reproducibility test
Delete all product code and re-run the full factory from the spec. Then **change the spec** (e.g. pull the uTP stretch goal into scope, or change the SSE contract) and see how cleanly the factory absorbs the change. This is the real measure of a factory compared with a one-off build.

### Metrics to track per run

| Metric | Why it matters |
| --- | --- |
| % of acceptance IDs green (by tier) | Output quality |
| Holdout / interop failures after the gate passed | Escaped defects, i.e. harness gaps |
| Human interventions (count and reason) | Autonomy; each one is a factory bug to fix |
| Attempts per ticket, gate failures by tier | Where the factory struggles |
| Wall-clock time and token cost | Efficiency; compares the approaches |
| Harness/test edits by non-verifier roles | Gaming indicator (should be 0) |
| Contract change requests | Quality of up-front decomposition |

After each run, hold a short retro: *for each human intervention, what change to the spec, harness, conventions or process would have prevented it?* Making that change is the core iteration loop.

---

## 7. Pitfalls to watch for

- **Theatre roles.** A "PM agent" that rewrites the spec in other words adds cost and no signal. Every role needs a distinct artifact and a measurable contribution.
- **Test gaming.** Implementers weakening assertions, skipping tests, or special-casing fixtures. Enforce ownership through permissions, not prompts.
- **Green but wrong.** The harness passes and the product still doesn't work, for example because every test runs against your own fakes. External oracles (L5) and E2E (L6) are what stop this.
- **Integration debt.** Parallel agents each "succeed" alone and fail together. Fix this with contract tests and frequent merges to a gated main. Don't use long-lived branches.
- **Context drift.** Long-running agents forget decisions. Keep durable state in files (ADRs, `PROGRESS.md`, backlog), not in conversation history.
- **Flaky gates.** Network-heavy tests (DHT, trackers) that are flaky teach agents to retry instead of fix. Keep L0–L4 hermetic and deterministic (loopback only, seeded randomness), and keep the real network out of the merge gate.
- **Optimizing the product instead of the factory.** The temptation to "just fix this one thing by hand" is strong. Do it only if you also record why the factory missed it, and fix that cause.

---

## 8. Questions this experiment should help you answer for your organization

1. How much human effort moves into specs and harnesses, and is it less in total than prompt-based development?
2. Which roles really add value, and which are overhead?
3. What escalation policy keeps humans involved for the right reasons only?
4. How do you make harness quality visible and reviewable? It replaces code review, so it needs its own review culture.
5. What changes when you change the spec mid-flight? This matters most for real products, which never have a frozen spec.
6. What does a "factory definition" look like as a reusable asset (agent roles, hooks, gate tiers, metrics) that other teams could adopt?
