# AC-06 points at a test package that does not exist
- Role: implementer   Ticket/IDs: AC-06 (M4)
- Blocking: yes, for `harness/factory gate --upto 4` exiting 0

## What I found
`harness/factory gate --id AC-06` reports `missing: package directory ./tools/swarm does not exist`.
There is no `TestStopSeeding` anywhere in the repo. Nothing in `harness/_staged/` provides it either.

## What the spec/harness says
`spec/acceptance.yaml` AC-06: `check: {go: ./tools/swarm, run: TestStopSeeding}` (product module, not `module: harness`).
`CLAUDE.md`, "Definition of done": "A test only counts if it is defined in a verifier-owned file."
The verifier-owned product-tree patterns are `*_acceptance_test.go`. The implementer can't write those.

## Options I see
1. The verifier adds `tools/swarm/stop_seeding_acceptance_test.go` (with a `TestStopSeeding`), or stages it and promotes it.
2. Repoint AC-06 at the existing swarm test that already covers it: STOP-01 (`./acceptance/swarm`, `module: harness`, `TestStopAndResumeSeeding`). That test checks the same behaviour: no more upload bytes, a `stopped` announce, files kept, and resume back to Seeding.

## What I did meanwhile
Nothing in the product tree. Writing a test there wouldn't count, and it would only fake the gate. The behaviour itself is implemented, and STOP-01 and the session contract test `TestContractSessionLifecycleActions` cover it.
