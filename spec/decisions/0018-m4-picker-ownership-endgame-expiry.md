# ADR-0018: M4 picker: block ownership, endgame, request expiry, contributors

- **Status:** Accepted (2026-11-08, architect, Approach A+ on M4). Supersedes the M3 selection rules of ADR-0012.
- **Spec:** §4.1 Piece selection, §16.2 A5, G10, D1 · **Acceptance:** PICK-01, PICK-02, PIPE-01, BAN-01, POISON-01, CPU-01
- **Contract:** `internal/bt/piece/contract.go`

## Context

M3's picker had four known defects:
- it accepted blocks nobody requested;
- it had no endgame and no timeout, so a peer that holds blocks and sends only keep-alives stalls the download;
- it sorted every piece on each `Pick`;
- it couldn't say who contributed to a failed piece.

M4 also needs random-first, strict priority classes (for M6) and banning. Hidden-test experience showed one more trap. When several peers contribute to one piece and one of them is corrupt, "every contributor gets a strike" (ADR-0006) can ban the innocent peer as well, and then the download livelocks (BAN-01 runs three rounds for exactly this).

## Decision

1. **Ownership.** A block is outstanding for a set of peers, each with a request time. `Received` accepts a block only from a peer it was requested from. The one exception is a late answer from a peer that `Release` or `Expire` took it back from, and only while the block is still needed. Anything else is wasted and never written.
2. **Selection order.** Strict priority class first. Within a class:
   - a peer first finishes the pieces it already owns;
   - then fresh pieces: random while fewer than 4 are verified, rarest-first after that;
   - then pieces other peers started;
   - then endgame.

   **Piece affinity** keeps a piece mostly with one peer, which keeps innocent peers out of hash-failure strikes.
3. **Endgame** starts when no wanted block is still needed, meaning every remaining block has been requested at least once (A5). `Pick` then hands out blocks outstanding elsewhere, up to `EndgameMaxOwners = 3` owners per block, and never twice to the same peer. `Received` returns the other owners in `Receipt.Cancel` and the session sends them `cancel` (PICK-02).
4. **Expiry.** A block outstanding for `DefaultRequestTimeout` (20 s) is taken back by `Expire()`, which the actor calls at least once a second; a cancel is sent for it. The stalled peer gets no new blocks until it delivers one, or until the cooldown has run out. Stalling therefore costs at most one timeout per round and can't hoard blocks.
5. **Contributors are origins, not keys.** `AddPeer(key, ip)`. `PieceFailed` returns the distinct IPs that contributed, including peers that have already disconnected. The session therefore doesn't need to keep a key→IP map for dead connections. A failed piece becomes **exclusive**: it is re-downloaded from a single peer, so a second failure blames exactly one IP.
6. **Cost.** No per-call scan or sort of all pieces. The contract test compares 2048 vs 16384 pieces and fails above 24× (quadratic would be about 64×).
7. **Determinism.** All randomness comes from `Options.Rand` and time from `Options.Clock` (T1/T2). `New(l, Options)` is the M4 constructor, and `NewPicker(l, rnd)` stays as a thin wrapper.

## Consequences

- PICK-01 (rarest-first after random-first-4) and "no peer is asked for a piece it doesn't have" are enforced at the unit level.
- Endgame duplicates waste a little bandwidth near the end; the cancels limit it.
- Re-requesting is a first-class operation (expiry, release, endgame), so later milestones (BEP 6 reject, M8) can add triggers without changing the model.
- File priorities (M6) only need `SetPriority`; `Done` already ignores skip pieces.
