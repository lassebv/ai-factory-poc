# `integrity --base main --role implementer` flags the architect's contract commits on the integration branch
- Role: implementer (integrator, Approach C)   Ticket/IDs: M3 definition of done #2
- Blocking: no for the product, yes for the literal DoD check

## What I found
`harness/factory integrity --base main --role implementer` exits 1 and lists 18 protected files: every `contract.go` / `contract_test.go` of M3, ADRs 0012–0015, `spec/decisions/README.md` and `spec/plans/m3.json`.

None of them were changed by an implementer or by the integration. They all come from the architect's 4 commits (`9419c01`, `fc946b3`, `3bab5db`, `8ed1518`). Those commits were made on `impl/m3-c` (and inherited by the WP branches), but were never merged into `main`. So the diff against `main` attributes them to whichever role runs the check.

Evidence:
- `git diff --stat 8ed1518 HEAD -- '*contract.go' '*contract_test.go' spec/` is empty.
- `harness/factory integrity --base 8ed1518 --role implementer` → `integrity ok: 32 changed files, all writable by role implementer`.

## What the spec/harness says
- `CLAUDE.md`, Definition of done 2: "`harness/factory integrity --base main --role <your role>` passes."
- Integrator prompt, Definition of done 2: "`harness/factory integrity --base main --role implementer` passes."
- `CLAUDE.md` Roles: the contracts area is "human, architect".

## Options I see
1. (Best) Process: merge the architect's contract commits into `main` (or onto a `contracts/m3` base) before the fan-out, and run implementer integrity against that base.
2. Harness: let `integrity` take a role per commit range (e.g. `--role architect` up to a given commit, `implementer` after it), or attribute each commit by a trailer.
3. Run integrity twice: `--base main --role architect` up to `8ed1518`, then `--base 8ed1518 --role implementer`.

## What I did meanwhile
Ran `--base 8ed1518 --role implementer` (passes; `--base main --role architect` also passes, since the architect may write both areas) and recorded the evidence above. I didn't change any protected file, and I didn't rebase or rewrite the architect's commits.
