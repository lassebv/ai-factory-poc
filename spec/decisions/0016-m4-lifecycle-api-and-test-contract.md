# ADR-0016: Milestone 4 pulls the lifecycle actions forward, plus the M4 test contract

- **Status:** Accepted (2026-11-08)
- **Spec:** §15.4, §5, §8.1 · **Acceptance:** M4 entries (SEED-*, CHOKE-01, PICK-*, BAN-01, RES-*, STOP-01, PAUSE-01, LIFE-04, …)

## Context

M4 delivers seeding, choking, the full picker, banning, persistence and stop-seeding (§15.4). Testing that black-box needs the lifecycle actions, which are officially M6. It also needs a few externally observable behaviors to be pinned down.

## Decision

1. **The lifecycle actions move forward.** M4 implements `POST /api/v1/torrents/{id}/pause`, `/resume` and `/stop-seeding` exactly as in `spec/openapi.yaml`, with the G18 validity rules (§16.4). `GET …/{id}` must report `stopReason` and `uploadedTotal`. The other endpoints (recheck, reannounce, queue, PATCH, DELETE, files, trackers, bulk) stay in M6.
2. **Existing data is verified on add.** Adding a torrent whose data is already in the download dir goes to `checking`, verifies what's there, and ends in `seeding` if everything is present (§5.3). That's how a torrent gets seeded from local files.
3. **Persistence.** Torrents, `desired_state`, `stop_reason`, the bitfield, the resume fingerprint and lifetime totals are stored (§7) and restored on startup (§6.1 step 4, §16.3 C3, G8).
4. **Peers from trackers** may be IPv6 literals in the dict (non-compact) peer list. Banning is per IP (G10). The BAN-01 test puts the good peer on `[::1]`, so a ban on `127.0.0.1` can't hit it.
5. **No test hooks for time.** CHOKE-01 observes real time: 10 s re-evaluation, 30 s optimistic rotation. That keeps the product free of test-only hooks, at the cost of a test that takes about 40 seconds.

## Consequences

- M6 shrinks again, since the three actions are done.
- **The fakes behind the M4 harness are validated against real libtorrent.** The fake leecher downloads from libtorrent, and its observation of choking matches libtorrent's own count (`harness/acceptance/oraclecheck`).
- **The persistence checks could not be validated against a reference implementation beforehand.** They fail for the right reason on the M3 code, and after the M4 runs they get mutation-tested.
