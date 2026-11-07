# STOR-02 (TestPartFiles) only passes with in-order piece picking
- Role: implementer   Ticket/IDs: STOR-02, §4.1 Piece selection, §16.2 A5
- Blocking: no (M3 passes with the interpretation below); it will matter at M4

## What I found
`TestPartFiles` uses a fake peer with `StallFrom: 4`. That peer answers
requests **strictly in arrival order** from one queue (`fakes/peer.go`, the
writer goroutine): once it reaches a request for piece ≥ 4 it blocks until
`Release()`, and every request queued behind it waits too. The test calls
`Release()` only after `a.bin` and `b.bin` (pieces 0–3) have their final
names.

If torrentd requests any block of piece 4 or 5 before the last block of
pieces 0–3 (with random-first, for example), the peer never sends pieces 0–3,
`a.bin`/`b.bin` never complete, and the test times out after 30 s. So the
check implicitly requires that, from a single peer, pieces are requested
in ascending order.

## What the spec/harness says
- §4.1 Piece selection: "Random first pieces until 4 are complete, then
  **rarest-first**".
- §16.2 A5: "Random-first-4 applies within the highest available class."
- The M3 brief: "Random-first / rarest-first can be simple at M3; M4 refines
  it."

With random-first-4 (§4.1/A5), STOR-02 fails randomly (whenever piece 4 or 5
is among the first picks). The test also depends on a client never having a
request for a stalled piece ahead of the others in the pipeline. That is an
artifact of the fake's single FIFO queue, not a protocol requirement.

## Options I see
1. Change the fake so that a stalled request doesn't block the requests queued
   behind it (serve the non-stalled requests, hold only the stalled ones).
   Then STOR-02 works with any picking order. This is my recommendation.
2. Keep the fake as it is and make "sequential within equal rarity, no
   random-first at M3" the documented M3 behavior. M4's random-first-4 would
   then break STOR-02.

## What I did meanwhile
At M3 the picker is rarest-first with ties broken by the lowest index, with no
random-first phase (allowed by "can be simple at M3"). Pieces already in
progress are finished first. From a single peer this downloads in order, so
STOR-02 passes deterministically. Recorded in PROGRESS.md.
