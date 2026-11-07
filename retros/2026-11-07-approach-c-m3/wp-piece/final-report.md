`internal/bt/piece` is finished and committed on `impl/m3-c-piece`, and every item in the definition of done passes. The frozen contract tests and my own unit tests pass with `-race`, and coverage is 100%.

## What I built
- **`bitfield.go`**: the Bitfield methods, in wire order (piece 0 is the high bit of byte 0).
  - `FromBytes` rejects a wrong length, a set spare bit or a negative `n` with `ErrInvalidBitfield`.
  - `Has`, `Set` and `Clear` out of range never panic. Every method also tolerates a nil pointer.
- **`picker.go`**:
  - **Geometry:** `Layout.PieceSize` and `Layout.NumBlocks`. A layout whose `TotalLength` is too small gives size 0 instead of a negative size.
  - **Pipeline depth:** `pipelineDepth` is clamp(rate·2/16 KiB, 5, 250), written so huge rates can't overflow.
  - **Picker:** the M3 picker, built on per-piece state: needed, in progress, pending or verified.
- **Pick order:** pieces already in progress come first, lowest index first. Then new pieces go rarest first, with ties by lowest index. There is no random-first phase and no endgame, so a block outstanding for one peer is never handed to another.
- **Efficiency:** each `Pick` passes over the pieces once and sorts the candidates. It never scans every block in the torrent, and block arrays exist only for pieces in progress or pending. In a benchmark of the gate's case (4 MiB torrent, 256 KiB pieces, a 250-deep pipeline topped up one block at a time), the whole download takes about 27 µs.
- **`piece_test.go`**: my own unit tests on top of the contract tests. They cover edge cases, availability bookkeeping, release behaviour, a failed piece being re-downloaded, a three-peer full download, and the benchmark.
- **`notes/piece.md`**: my decisions and interpretations.

## Results
- `go test -race -cover ./internal/bt/piece/...` passes with 100.0% coverage.
- `go vet`, `gofmt` and staticcheck are clean for the package.
- `harness/factory integrity --base 8ed1518… --role implementer` passes (5 changed files, all writable by the implementer role).
- Two commits: the implementation, then the tests and notes.

## Escalations
None. The contract was enough and I didn't need any workarounds.

## For the integrator
- The picker is not safe for concurrent use, so only the torrent actor may call it.
- **Pending pieces:** after `Received` reports a piece as done, nobody picks that piece again until you call `PieceVerified` or `PieceFailed`.
- **`PieceFailed`** only acts on a pending piece and ignores any other state. The failed piece then counts as a new piece, ranked by rarity rather than put first.
- **`PieceVerified`** accepts a piece in any state, so you can use it for pieces found during checking. It also drops that piece's outstanding blocks from each peer's count, so `Outstanding(p)` can go down without a `Received` call.
- **Released pieces:** after `Release` or `RemovePeer`, a piece with nothing outstanding and nothing received counts as new again. A piece with at least one received block stays in progress.
- **`Pick` return value:** it returns nil for an unknown peer, for `max <= 0`, or when there is nothing to request.
- **Pieces you don't have yet:** `Received` doesn't check which peer sent the block. A valid block of a piece nobody has started is accepted and starts that piece.
