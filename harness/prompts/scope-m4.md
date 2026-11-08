**Milestone 4 (Swarm engine)**, §15.4 + ADR-0016. Milestones 1–3 are merged. You're building on the M3 engine (`internal/bt/{tracker,peer,piece,storage,session}`), which was built with frozen `contract.go` files. Those contracts describe M3. **Evolving them for M4 is expected.** Whoever owns the contracts in your setup changes them deliberately, and records each change in an ADR.

What M4 adds:

- **Seeding.**
  - The peer port serves incoming connections for active torrents.
  - Requests are answered from storage.
  - `HAVE` is sent for newly verified pieces.
  - Uploads count in `uploadedTotal`.
  - Adding a torrent whose data already exists verifies it (`checking`) and goes to `seeding` (§5.3).
- **Choking (§4.1):**
  - tit-for-tat with 4 unchoke slots by rate, re-evaluated every 10 s;
  - an optimistic unchoke rotated every 30 s;
  - seeding mode unchokes by upload rate;
  - anti-snubbing after 60 s without data.
- **The picker (§4.1, A5):**
  - random-first for the first 4 pieces, then rarest-first, using availability from bitfields and `have` messages;
  - strict priority classes, ready for M6;
  - **endgame mode:** request the remaining blocks from all peers and cancel duplicates;
  - per-peer block ownership, plus a request timeout or re-request, so a stalled peer can't hold blocks forever.
- **Adaptive request pipelining (D1):** clamp(rate × 2 s / 16 KiB, 5, 250).
- **Hash failures and banning (§4.1, G10, ADR-0006):**
  - record which peers contributed to each piece, and give each contributor a strike on failure;
  - ban an IP at 3 strikes (24 h expiry, persisted in `banned_peers`), then disconnect it and never reconnect to it;
  - a peer that corrupts everything must never stall the download.
- **Persistence and fast resume (§6.1 steps 3–4, §6.2, §7, §16.3 C3, G8, ADR-0002):**
  - torrents, `desired_state`, `stop_reason`, the bitfield, the resume fingerprint (per-file size + mtime) and lifetime totals are stored, and flushed every 10 s and at shutdown;
  - on startup, a clean shutdown with matching fingerprints means no recheck; changed files mean a recheck; an unclean shutdown means size checks only;
  - all files missing means `error` "Files missing".
- **Lifecycle (§5, ADR-0016):**
  - `POST …/pause`, `…/resume` and `…/stop-seeding`, with the G18 rules;
  - stop seeding sends `stopped` announces, closes peers, and moves the torrent to `stopped` with `stopReason: user`;
  - resume seeding goes back to `seeding`.
- **Concurrency and resources (§4.5):**
  - bounded, process-wide disk and hash worker pools (defaults 4 and GOMAXPROCS/2), not one goroutine per block;
  - no leaked goroutines or connections; there's an in-repo goroutine-leak helper for tests;
  - bounded memory with large pieces;
  - `-race` clean.
- **Shutdown (§16.11):** the `stopped` announces start at the same time as the HTTP drain.

**Out of scope:** UDP trackers, DHT, PEX, LSD and magnets (M5); seeding limits, queueing, settings, SSE and the remaining endpoints (M6); the UI (M7); BEP 6, IPv6 announces and interop beyond SEED-02 (M8).

**Known issues in the M3 code you inherit** (from independent reviews and hidden tests):
1. There's no endgame and no request timeout: a peer that holds blocks and only sends keep-alives stalls the download.
2. Disk writes and hashing use one goroutine per block, behind per-torrent semaphores, instead of §4.5's process-wide bounded pools.
3. `piece.Picker.Received` accepts blocks that were never requested from that peer.
4. `Pick` sorts every needed piece on each call, and `fillAll` runs on every received block.

**Notes:**
- **CHOKE-01** watches real time (about 40 s).
- **SEED-02** needs Docker (a libtorrent leecher).
- **BAN-01** puts the good peer on `[::1]`. Tracker dict peer lists may contain IPv6 literals.
