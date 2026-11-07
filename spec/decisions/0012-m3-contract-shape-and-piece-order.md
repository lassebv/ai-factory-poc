# ADR-0012: M3 contract shape, and in-order piece selection at M3

- **Status:** Accepted (2026-11-07, architect, Approach C run on M3)
- **Spec:** §4.1 Piece selection, §16.2 A5, D1 · **Acceptance:** STOR-02, WIRE-05

## Context

Approach C needs package contracts that parallel implementers cannot drift from, and that compile before any implementation exists (`go vet ./...` compiles the contract tests).

STOR-02 (`TestPartFiles`) uses a fake peer that answers requests strictly in order and blocks its whole queue on the first request for piece ≥ 4. The test only passes if every block of pieces 0–3 is requested before any block of pieces 4–5. §4.1 asks for "random first pieces until 4 are complete, then rarest-first", which would make that test fail at random.

## Decision

1. **Contract shape.** Each package's `contract.go` declares interfaces, shared types, constants and errors, plus each constructor as an exported one-line wrapper over an unexported function (`func NewPicker(...) Picker { return newPicker(...) }`). The signature is therefore frozen in the contract file, and the implementer writes only `newPicker`. A `stub.go` with panicking placeholders keeps the tree compiling until the implementer deletes it. Exported methods on concrete contract types (`piece.Bitfield`, `piece.Layout`, `tracker.Event`) are listed in the package doc and stubbed the same way.
2. **Piece order at M3.** The picker serves pieces already in progress first (lowest index), then new pieces rarest-first with ties broken by the lowest index. There is no random-first phase and no endgame at M3. The picker accepts a `*rand.Rand` now, so M4 can add random-first without changing the signature.
3. **Blocks and pipelining.** Blocks are 16 KiB (shorter only at a piece end). `piece.PipelineDepth(rate)` implements D1, `clamp(rate*2s/16 KiB, 5, 250)`.

## Consequences

- With a single seeder, the download is sequential, which is what STOR-02 needs.
- M4 must reconcile random-first with STOR-02. See `escalations/M3-piece-order-vs-random-first.md`.
- Implementers cannot change a constructor's signature without the change showing up as an edit to `contract.go`, which the write policy blocks.
