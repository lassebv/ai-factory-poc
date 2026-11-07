# piece (M3, Approach C) — implementer notes

Files: `internal/bt/piece/bitfield.go`, `picker.go`, `piece_test.go` (stub.go deleted).

## Decisions and interpretations

- **Per-piece state.** Each piece is `needed`, `active` (≥1 block outstanding or
  received), `pending` or `have`. Block arrays (`blocks`, `owner`) are allocated
  only while a piece is active/pending, so memory is O(blocks in flight).
- **Pick cost.** One pass over pieces for in-progress pieces (lowest index), then
  collect new candidate pieces the peer has and sort by (availability, index).
  Per call: O(P log P + blocks of touched pieces). Never scans every block of the
  torrent. Benchmark: 4 MiB / 256 KiB, 250-deep pipeline topped up one at a time,
  ~27 µs for the whole download.
- **Released piece goes back to "new".** If a Release/RemovePeer leaves an active
  piece with nothing outstanding and nothing received, it returns to `needed` and
  is ranked by rarity again (the contract's definition of in-progress is "a block
  outstanding or received"). A piece with a received block stays in progress.
- **PieceFailed** only acts on a pending piece; other states are ignored. The
  piece returns to `needed` (no blocks outstanding/received), i.e. it is ranked as
  a new piece.
- **PieceVerified** accepts any state, and also drops any still-outstanding blocks
  of that piece from their peers' outstanding sets (so `Outstanding` shrinks).
- **Received from an unrequested peer** of a `needed` piece activates it; the
  peer key is not consulted (the contract says it need not be outstanding for p).
- **Pick returns nil** (not an empty slice) when nothing is requestable, for an
  unknown peer, or for max ≤ 0.
- **Bitfield** methods are nil-safe; NewBitfield(n<0) is length 0; FromBytes with
  n<0 is ErrInvalidBitfield.
- **PipelineDepth** computes floor(rate*2/BlockSize) without overflow for huge rates.
- **Inconsistent Layout** (TotalLength too small): PieceSize clamps to 0, such
  pieces have 0 blocks and are never picked.
- `rnd` is stored but unused at M3 (reserved for M4 random-first, ADR-0012).

No escalations; the contract was sufficient.
