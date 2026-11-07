# Factory harness

The harness is how the factory decides whether `torrentd` works, without anyone reading the code. It is a separate Go module (`torrentd.local/harness`). It is not part of the product and not subject to depcheck.

```
harness/
  factory              entry point: builds bin/factory on demand, then runs it
  cmd/factory/         CLI: gate, validate, integrity, hook, policy, lint-clock, quality, coverage
  internal/
    acceptance/        loads and validates spec/acceptance.yaml
    gate/              runs checks; only trusts verifier-owned tests
    policy/            role → writable paths (single source of truth)
    hook/              Claude Code PreToolUse hook (fast feedback)
    integrity/         git-diff check at merge time (enforcement)
    static/            clock lint, doc-comment/file-length check, coverage floors
  hooks/               pre-tool-use.sh (wired up in .claude/settings.json), build.sh
  prompts/             versioned run prompts + run-single-agent.sh launcher (Approach A)
  _staged/m<N>/        package-level acceptance tests for milestones not started yet (Go ignores _dirs)
  checks/*.sh          script checks referenced by spec/acceptance.yaml
  acceptance/          black-box Go acceptance tests that build and run ./cmd/torrentd
    internal/app/      launcher: build once, start with temp HOME/data dir/port, wait for /healthz
    internal/oas/      minimal JSON-Schema validator for spec/openapi.yaml
    skeleton/          M1: healthz, migrations, WAL, SPA fallback, config, Host/Origin, dev mode
    depcheck/          M1: tools/depcheck against module-graph fixtures
    preview/           M2: metainfo validation via POST /api/v1/torrents/preview
  fixtures/
    torrents/          real, libtorrent-corpus and synthetic .torrent files, plus expected.json
    depcheck/          go.mod + `go mod graph` fixtures
  oracle/              Docker image with Transmission + libtorrent (independent oracles)
```

Package-level acceptance tests for the pinned APIs are verifier-owned `*_acceptance_test.go` files. Until their milestone starts they wait in `harness/_staged/m<N>/`; `harness/factory promote --ms <N>` moves them into the product tree (e.g. `internal/bt/bencode/`). Committing them early breaks `go vet ./...` and coverage for the current milestone (finding from the M1 run). Playwright tests live in `e2e/`.

## Running

```sh
harness/factory validate                 # schema-check spec/acceptance.yaml
harness/factory gate --ms 1              # gate: exit 1 unless every selected check passes
harness/factory gate --upto 2 --report   # progress report (always exit 0)
harness/factory gate --tier L0,L1 -v     # filter by tier; -v prints full failures
(cd harness && go test ./internal/... ./acceptance/internal/...)   # the harness's own tests
```

Each gate run writes `runs/gate/<timestamp>.json` and `runs/gate/latest.json`. The statuses are:
- `pass`
- `fail`
- `missing`: the test, file or script doesn't exist yet
- `error`: the harness couldn't run the check, e.g. `node_modules` is missing
- `manual`

Playwright needs `npm ci` in `e2e/` and `npx playwright install chromium`, and it uses port 8080. Both are already done on this machine.

## Why the gate can be trusted

- **Only verifier-owned tests count.** A `go` check passes only if the named test is defined in a `*_acceptance_test.go` file, or under `harness/acceptance/`. An implementer can't satisfy `TestHealthz` by writing their own.
- **Missing, skipped or unbuilt tests never pass.**
- **Expectations come from independent oracles.** Info-hashes, names, sizes and file lists in `fixtures/torrents/expected.json` come from libtorrent and are cross-checked against Transmission (`oracle/`). They were never computed by torrentd. Both oracles agree on every hash, and the real-world hashes match their published values (for example, Sintel `08ada5a7…`).
- **Black-box first.** Most checks drive the real binary over HTTP, the CLI and the filesystem, so they don't constrain the internal design. Package-level checks only touch the minimal APIs in `spec/contracts/go-api.md`.
- **Ownership is enforced twice:** by the hook as agents work, and by `factory integrity` at merge time. Every hook denial is logged to `runs/policy-denials.jsonl`, which serves as a gaming indicator.

## How the harness was validated (2026-11-07)

A throwaway reference implementation of milestones 1–2 was written **outside the repo** (in a scratch copy), only to prove the checks can pass:

- **Pass rate:** all 39 M1–M2 entries passed except QUAL-01, the coverage floor. QUAL-01 failed correctly, because the throwaway code had no unit tests.
- **Mutation testing:** five deliberately introduced bugs were each caught:
  - Host check removed: SEC-01 failed.
  - Strict mode accepting unsorted keys: BENC-02 and META-10 failed.
  - Pad files shown: META-01 and META-07 failed.
  - `..` allowed in paths: META-02 failed.
  - Wrong exit code: SKEL-07 failed.
- **Contract fixes:** this exercise found and fixed three contract issues before any factory run:
  - depcheck now handles real `go mod graph` output (go/toolchain pseudo-modules, `// indirect` requirements, transitive dependencies of `golang.org/x`).
  - The warning text accepts either `[::]` or `0.0.0.0`.
  - The meaning of "nesting depth" is spelled out.

## Adding checks (verifier role)

1. Add an entry to `spec/acceptance.yaml`. Prefer `{go: ./acceptance/<pkg>, module: harness, run: TestX}`.
2. Write the test under `harness/acceptance/`, a fixture under `fixtures/`, or a script under `checks/`.
3. Prove the test can fail: break the behavior on purpose and watch it go red.
4. To regenerate the torrent fixtures and expectations, run `fixtures/torrents/regen.sh` (needs Docker). Verdicts live in `fixtures/torrents/build_expected.py`.

## Known limitations

- The Bash part of the hook parses commands for their real write targets: redirections, `tee`, `rm`/`mv`/`cp`/…, `sed -i`, `git checkout -- <path>`, `bash -c` and heredoc-fed shells, and literal paths in inline Python/Node code. Heredoc bodies and quoted text are content, not targets. A write through a variable path inside an inline script is not caught. The integrity check is the real enforcement. (v1 blocked any write command that merely mentioned a protected path; that caused 4 false positives in the M1 run.)
- Subagents of the interactive (human) session are `unknown`, so they can't write product code. Use the role-named agents.
- The NFR, interop and E2E checks for later milestones are placeholders. The verifier refines them before each milestone starts.
- `TestNonLoopbackWarning` binds `0.0.0.0`. On macOS the application firewall may ask once for permission.
