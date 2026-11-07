# ADR-0009: Code-quality gates

- **Status:** Accepted (2026-11-06)
- **Audit finding:** A14 · **Spec:** §16.10

## Context
§1.1 asks for "readable, well-tested code, suitable as a reference project". A gate cannot check that without numbers.

## Decision
**Blocking checks (part of the merge gate):**
- Coverage ≥ 80% per `internal/bt/*` package, ≥ 60% elsewhere.
- `go vet` and staticcheck are clean. Staticcheck is run outside `go.mod` (see §16.10 T5).
- Every exported identifier has a doc comment.
- No Go file is over 800 lines.

**Advisory only:**
- A reviewer agent scores each change against `CLAUDE.md`. The score is feedback and does not block merging.

## Consequences
- Quality becomes measurable and comparable across factory runs.
- Coverage can be gamed. Holdout tests and mutation testing (see `factory-approaches.md` §3.3) are the counterweight.
