# STOR-02 depends on in-order piece selection, which §4.1 random-first contradicts
- Role: architect   Ticket/IDs: STOR-02 (TestPartFiles), PICK-* (M4)
- Blocking: no (M3); yes for M4 unless resolved

## What I found
`TestPartFiles` uses `fakes.PeerOptions{StallFrom: 4}`. The fake peer serves requests strictly in order, from a single queue, and blocks that whole queue on the first request for a piece ≥ 4 until `Release()`. So pieces 0–3 complete, and `a.bin`/`b.bin` get their final names, only if every block of pieces 0–3 is *requested* before any block of piece 4 or 5. A random-first picker requests a random piece first, and with probability 2/6 that's piece 4 or 5, so the test fails at random.

## What the spec/harness says
- `project-description.md` §4.1, Piece selection: "Random first pieces until 4 are complete, then **rarest-first**".
- §16.2 A5: "Random-first-4 applies within the highest available class."
- `harness/acceptance/download/download_test.go` `TestPartFiles`: `p := e.peer(t, tor, fakes.PeerOptions{StallFrom: 4}) // pieces 4-5 are c.bin`.

## Options I see
1. In M4, make the fake peer stall *only* requests for pieces ≥ StallFrom, and keep serving the others (verifier change). That's the best option: the test then checks STOR-02, not the picker order.
2. Keep in-order selection forever for torrents with ≤ N pieces (that's a product hack).
3. Disable random-first when there is a single peer (that changes the spec).

## What I did meanwhile
ADR-0012: the M3 picker is deterministic. In-progress pieces come first, then rarest-first with ties broken by the lowest index, and there's no random-first phase. `internal/bt/piece/contract_test.go` pins this (`TestContractPickerInOrderForSinglePeer`). M4 must revisit it together with option 1.

Also noted: the WIRE-04 text says "200 peers", but `TestGarbageDoesNotCrash` uses 40 and requires ≥ 20 connections. I designed for the test (per-torrent max of 50 peers and max 20 half-open, §4.1 Connection limits); the text should say 40.
