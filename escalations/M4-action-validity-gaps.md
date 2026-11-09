# G18 is silent on several lifecycle actions
- Role: architect   Ticket/IDs: M4 / LIFE-04, PAUSE-01, STOP-01, RES-06
- Blocking: no
## What I found
G18 defines pause on complete torrents, stop-seeding on incomplete ones, and resume on active and error torrents. It doesn't define:
- pause on `paused` or `error`;
- stop-seeding on `stopped`, `checking` (completeness not known yet) or `error`;
- what `startPaused=true` means when the data on disk turns out to be complete. `paused` is defined as incomplete (§5.1), and pausing a complete torrent is `invalid_state`.
## What the spec/harness says (quote, with file:section)
- project-description.md §16.4 G18: "`pause` | Allowed in `metadata`, `checking`, `queued`, `downloading`. On a complete torrent: `409 invalid_state`." / "`stop-seeding` | On an incomplete torrent: `409 not_complete`." / "`resume` | On a torrent that is already active: `204`, no-op."
- §5.1: "`paused` | Incomplete and stopped by the user."
## Options I see
1. Treat repeats as idempotent no-ops (pause on paused, stop-seeding on stopped), refuse `error` with `invalid_state`, and treat `checking` as not complete.
2. Return `invalid_state` for every unlisted combination.
## What I did meanwhile
Chose option 1. It is consistent with resume being a no-op on active torrents, and it makes double-clicks harmless. `startPaused` over complete data ends in `stopped` with `stopReason: user`. All of this is recorded in `internal/bt/session/contract.go` (Torrent lifecycle) and ADR-0019 §5. LIFE-04 only exercises the listed cases, so either option passes the gate.
