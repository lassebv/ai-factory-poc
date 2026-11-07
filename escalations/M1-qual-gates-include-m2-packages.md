# QUAL-01/QUAL-02 (ms 1) can't pass while the M2 acceptance tests are in the tree
- Role: implementer   Ticket/IDs: QUAL-01, QUAL-02 (also DoD "go vet ./... is clean")
- Blocking: yes (for QUAL-01; QUAL-02 is worked around with API stubs, see below)

## What I found
`main` already contains two verifier-owned M2 test files:
`internal/bt/bencode/bencode_acceptance_test.go` and
`internal/bt/metainfo/magnet_acceptance_test.go`. They are in-package tests
against the pinned APIs (`Marshal`, `Unmarshal`, `UnmarshalStrict`,
`RawMessage`, `Magnet`, `ParseMagnet`).

Without M2 code these packages hold only test files that don't compile, so:
- `go vet ./...` fails (vet type-checks test files) → QUAL-02 fails, and
  so does definition-of-done item "go vet ./... is clean".
- staticcheck `./...` fails the same way → QUAL-02.
- `go test -cover ./...` reports `FAIL torrentd/internal/bt/bencode [build failed]`,
  which `harness/internal/static.Coverage` records as a finding
  ("tests fail") with an 80% floor → QUAL-01 fails.

Even with compiling stubs of the pinned APIs, the M2 acceptance tests run and
fail inside `go test -cover ./...`, so QUAL-01 still fails for
`internal/bt/bencode` and `internal/bt/metainfo`. The only way to make
QUAL-01 pass at M1 is to implement the M2 bencode and magnet packages, and
the M1 brief explicitly forbids that ("Don't start milestone 2").

## What the spec/harness says
- `spec/acceptance.yaml` QUAL-01: "Coverage is at least 80% per internal/bt/*
  package and at least 60% elsewhere (**enforced from the milestone that
  introduces each package**)." `internal/bt/bencode` and `internal/bt/metainfo`
  are introduced in M2 (§15.2).
- `harness/internal/static/static.go` `Coverage`: runs `go test -cover ./...`
  over everything, "Packages without tests count as 0%", no milestone filter.
- `harness/README.md`, validation: the harness was proven with a reference
  implementation of M1 **and** M2 together, so this M1-only state was never
  exercised.

## Options I see
1. Harness: make `factory coverage` (and the vet/staticcheck step) skip
   packages whose milestone is later than the gate's `--ms`/`--upto`. For
   example, map package → introducing milestone in acceptance.yaml, or skip
   packages that hold only `*_acceptance_test.go` files plus stubs.
2. Process: don't commit acceptance tests for milestone N+1 into product
   packages until milestone N+1 starts (keep them under `harness/` until then).
3. Scope: declare that M1 includes the pinned M2 APIs (bencode + magnet).

## What I did meanwhile
- I added minimal **compiling stubs** of the pinned APIs in
  `internal/bt/bencode` and `internal/bt/metainfo`. They have the exact
  pinned signatures and return a "not implemented (milestone 2)" error, so
  `go vet ./...` and staticcheck can type-check the tree. They contain no
  M2 logic.
- I did not weaken or skip any check. QUAL-01 is expected to stay red only
  for those two packages (their acceptance tests fail against the stubs).
  Every M1 package is at or above its floor.

Measured on `impl/m1-approach-a` (2026-11-07): `harness/factory gate --ms 1`
shows 17/18 passing. QUAL-02 passes thanks to the stubs. QUAL-01 fails with
exactly `internal/bt/bencode: tests fail` and `internal/bt/metainfo: tests
fail`. Everything else is above its floor: `.` 75%, api 99%, clock 100%,
config 100%, store 89%, depcheck 96%.

## Resolution (human, 2026-11-07): fixed in factory v1.1 (`9b8c18e`)
Option 2 (staging), plus milestone-aware coverage (option 1, as QUAL-01's text already said). The M2 package acceptance tests now live in `harness/_staged/m2/`. The verifier runs `harness/factory promote --ms 2` when M2 starts. Coverage floors are enforced from the milestone that introduces a package. The placeholder stubs stay until M2 replaces them. See `retros/2026-11-07-approach-a-m1/` (F2).
