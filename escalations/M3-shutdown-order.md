# §6.2 shutdown order vs the TRK-01 timing budget
- Role: architect   Ticket/IDs: TRK-01, §6.2
- Blocking: no

## What I found
§6.2 runs the graceful HTTP shutdown (up to 5 s) *before* the `stopped` announces (3 s). TRK-01 requires the `stopped` announce within 3.5 s of SIGTERM. Run strictly in sequence, a slow HTTP drain could use up the whole budget.

## What the spec/harness says
- `project-description.md` §6.2: "1. Graceful HTTP shutdown (5 s). 2. Send `stopped` announces to trackers, in parallel, with a 3 s timeout."
- `spec/acceptance.yaml` TRK-01: "event=stopped on SIGTERM within the 3 s shutdown budget". `TestAnnounceLifecycle` fails if it arrives more than 3500 ms after the signal.

## Options I see
1. Start `Session.Close`, which sends the `stopped` announces, at the same moment as the HTTP shutdown. Wait for both, then flush and close the DB. (Chosen.)
2. Run the announces strictly first, then the HTTP shutdown.
3. Keep §6.2 as written, and hope HTTP drains instantly (it usually does).

## What I did meanwhile
ADR-0015 §3 records option 1. Steps 3 and 4 (flushes, then closing the DB) still run last, as §6.2 says. The human should confirm, or amend §6.2.

## Resolution (human, 2026-11-08)
Option 1 is confirmed. It's normative in `project-description.md` §16.11, with a pointer at §6.2, and ADR-0015's status is updated. Holdout H12 verifies it: A, which followed §6.2 literally, sent its `stopped` announce 5.0 s after SIGTERM with an HTTP request in flight; C sent it at 0.0 s.
