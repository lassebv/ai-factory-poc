# "Every contributor gets a strike" can ban the good peer in BAN-01
- Role: architect   Ticket/IDs: M4 / BAN-01, G10, ADR-0006
- Blocking: no
## What I found
ADR-0006 gives every contributor of a failed piece a strike, and bans an IP at 3 strikes. In BAN-01, a fast corrupt peer and a slower good peer (on `[::1]`) download a 1 MiB torrent with 32 KiB pieces, which is 2 blocks per piece. With an M3-style picker, which shares in-progress pieces across peers, many pieces get one block from each peer. The good peer then collects strikes as fast as the bad one, and can be banned in the same round. The download then stalls. BAN-01 runs three rounds to catch exactly this intermittent livelock.
## What the spec/harness says (quote, with file:section)
- spec/decisions/0006-peer-ban-policy.md: "When a piece fails its hash check, **every contributor gets a strike**." / Consequences: "An innocent peer that shared pieces with a bad one may collect strikes."
- spec/acceptance.yaml BAN-01: "…the download completes from a good peer within 30 s, three times in a row (no intermittent livelock)."
## Options I see
1. Keep the strike rule, and make shared pieces rare in the picker: piece affinity in normal mode, and single-source re-download of a failed piece, so the second failure blames one IP.
2. Change ADR-0006 so that a failed piece with several contributors gives no strikes, or a fractional strike. That is a human decision.
## What I did meanwhile
Chose option 1 without changing ADR-0006. It is in the picker contract (ADR-0018 §2 and §5) and tested by `TestContractPickerPieceAffinity` and `TestContractPickerFailureContributors`. Some residual risk remains in endgame, where duplicates can mix contributors on the last pieces. The human may want to decide option 2 for v1 anyway.
