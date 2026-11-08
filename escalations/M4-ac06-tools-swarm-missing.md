# AC-06 points at a test package that does not exist (`./tools/swarm`)
- Role: architect + implementer (Approach A single agent)   Ticket/IDs: AC-06 (M4)
- Blocking: yes, for `harness/factory gate --upto 4` (AC-06 reports `missing`, so the gate exits 1)

## What I found
`spec/acceptance.yaml` has AC-06 with `ms: 4`, check `{go: ./tools/swarm, run: TestStopSeeding}`. It has no `module: harness`, so the gate looks for the package in the product module. `./tools/swarm` doesn't exist, and there's no staged test for it (`harness/_staged/` doesn't exist). The gate reports: `AC-06 4 L4 missing package directory ./tools/swarm does not exist`.

## What the spec/harness says
- `spec/acceptance.yaml` AC-06: "Stop seeding: no further upload bytes, a `stopped` announce reaches the tracker, files are kept; Resume seeding brings it back to Seeding."
- `CLAUDE.md`, Definition of done: "A test only counts if it is defined in a verifier-owned file." `*_acceptance_test.go` files are verifier-owned.
- `factory-approaches.md` §… mentions a future `tools/swarm` scenario DSL ("seeder S, leechers L1–L3 …"). That's a design idea, not a contract I can implement against.

## Options I see
1. **(Recommended)** The verifier re-points AC-06 at the existing black-box test that checks exactly this behavior: `{go: ./acceptance/swarm, module: harness, run: TestStopAndResumeSeeding}`. That test (STOP-01) passes. It checks `stopped` + `stopReason user`, connections closed, no further upload bytes, a `stopped` announce and files unchanged, and that resume brings the torrent back to seeding with a new leecher completing.
2. Move AC-06 to a later milestone, when the `tools/swarm` L4 harness exists.
3. The verifier writes `tools/swarm/*_acceptance_test.go` and a scenario DSL. I can't do this: the test file is verifier-owned, and inventing the DSL would be guessing.

## What I did meanwhile
- I implemented the behavior. STOP-01 (`TestStopAndResumeSeeding`) and the session unit test `TestSwarmSeedLeechLifecycleAndResume` cover it, and both pass.
- I did not create `tools/swarm` or any acceptance-test file. The rest of M4 is green. `gate --upto 4` fails only on this `missing` entry.
