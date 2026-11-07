# torrentd — software-factory experiment

This repo is an experiment in building software with a multi-agent factory. The product is `torrentd`, a single-binary BitTorrent client with a web UI. The factory is the spec, the harness, the roles and the process around it. **If the output is wrong, fix the spec, the harness or the process, then re-run. Don't hand-patch the code.**

## Sources of truth (in order of precedence)

1. `project-description.md`: product intent. **§16 overrides §1–§15.**
2. `spec/decisions/`: ADRs for the human-owned decisions.
3. `spec/openapi.yaml`: the HTTP contract. `spec/contracts/go-api.md`: the pinned Go APIs and the CLI contract.
4. `spec/acceptance.yaml`: requirement → executable check. This is the definition of done.
5. `spec/spec-audit.md`: the reasoning behind §16. Read it when §16 is unclear.

If these sources conflict, or are silent on something you need, **don't guess**. Write an escalation (see below).

## Roles and ownership

Your role is your subagent type (`.claude/agents/<role>.md`), or `FACTORY_ROLE` for headless workers. An interactive session without either is `human`. Any other agent type is `unknown`, and can only write to the shared areas.

| Area | Paths | Who may write |
| --- | --- | --- |
| intent | `project-description.md`, `factory-approaches.md`, `spec/spec-audit.md` | human |
| factory machinery | `CLAUDE.md`, `.claude/`, `harness/{cmd,internal,hooks,prompts}/`, `harness/go.*`, `harness/factory`, `harness/README.md`, `retros/` | human |
| acceptance | `spec/acceptance.yaml`, the rest of `harness/` (including `harness/_staged/`), `e2e/`, `*_acceptance_test.go`, `*.acceptance.test.ts(x)` | human, verifier |
| red team | `harness/redteam/` | human, verifier, red-team |
| contracts | everything else in `spec/` (decisions, OpenAPI, Go API) | human, architect |
| product | everything else | human, architect, implementer |
| shared | `escalations/`, `reviews/`, `runs/` | everyone |

This is enforced in two places:
- A PreToolUse hook (`harness/hooks/pre-tool-use.sh`) blocks writes as you work.
- `harness/factory integrity` fails the merge if a branch touched files its role doesn't own.

`harness/internal/policy/policy.go` is the single source of these rules. **If a write is blocked, don't work around it.** Escalate.

## Definition of done for a work unit

1. The acceptance entries for the unit pass: `harness/factory gate --id <IDs>`, or `--ms <N>` for a milestone.
2. `harness/factory integrity --base main --role <your role>` passes.
3. New product code has its own unit tests, and the coverage floors hold: `harness/factory coverage` requires ≥ 80% for `internal/bt/*` and ≥ 60% elsewhere.
4. `harness/checks/quality-check.sh` passes: vet, staticcheck, doc comments on exported identifiers, no Go file over 800 lines.
5. No new dependencies outside the §2 allowlist: `harness/checks/depcheck.sh`.

A test that is missing, skipped or doesn't build never counts as a pass. A test only counts if it is defined in a verifier-owned file.

**Scope beats the definition of done.** If a check can't pass without going beyond your assigned scope, don't expand the scope. Escalate, finish everything else, and say so in your report.

**Staged tests.** Package-level acceptance tests for milestones that haven't started live in `harness/_staged/m<N>/` (Go ignores `_` directories). The verifier promotes them into the product tree with `harness/factory promote --ms <N>` when that milestone starts. Coverage floors apply from the milestone that introduces a package.

## Commands

```sh
harness/factory gate --ms 1          # run milestone 1's acceptance checks (results in runs/gate/)
harness/factory gate --upto 2 --report   # progress report, always exits 0
harness/factory gate --id SKEL-01,SEC-02 -v   # specific checks, full failure output
harness/factory integrity --base main --role implementer
harness/factory policy implementer internal/api/server.go   # explain a write decision
harness/factory promote --ms 2       # verifier/human: move staged M2 tests into the tree
harness/prompts/run-single-agent.sh 2   # human: launch an Approach A run for milestone 2
```

**Write file contents with the Write/Edit tools.** Use Bash to run commands, not to write files. The hook only checks real write targets, but Write/Edit is the reliable path and shows up cleanly in the transcript.

## Engineering conventions

- **Language and module:** Go 1.25, product module at the repo root. The recommended module path is `torrentd`. Acceptance tests don't depend on the module path.
- **Package layout:** as in §4.4, plus `cmd/torrentd`, `internal/{api,stats,store,config,clock}`, `tools/depcheck` and `webembed.go`.
- **Dependencies:** stdlib, `golang.org/x/*` and `modernc.org/sqlite` only (§2). Never add a `tool` directive to `go.mod`. Run dev tools with `go run pkg@version`.
- **HTTP:**
  - Stdlib `net/http` with method patterns.
  - Every error uses the `Error` schema in `spec/openapi.yaml`.
  - Host and Origin checks run before routing (§16.7 G17).
- **Logging:** `log/slog` with a `TextHandler` on stderr.
- **Time and randomness:** no `time.Now`, `time.After`, `time.Sleep` etc. in `internal/bt/**`, `internal/stats` or `internal/store`. Use the injected `internal/clock` (T1). Randomness comes from an injected source (T2).
- **Concurrency:** `context.Context` everywhere, and §4.5's actor model. Peer goroutines never touch `Torrent` state; they send it events.
- **Tests:** hermetic. Loopback only, no public network, deterministic seeds.

## Escalations

Write `escalations/<ticket-or-ID>-<short-slug>.md` when any of these apply:
- the spec is ambiguous or contradictory;
- you believe an acceptance check or fixture is wrong;
- you need a contract change (OpenAPI, Go API, schema);
- you are stuck after 3 serious attempts.

Use this template:

```md
# <title>
- Role: <your role>   Ticket/IDs: <...>
- Blocking: yes/no
## What I found
## What the spec/harness says (quote, with file:section)
## Options I see
## What I did meanwhile
```

Then carry on with unblocked work. Never weaken a check to make progress.
