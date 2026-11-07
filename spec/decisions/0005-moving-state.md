# ADR-0005: Add a `moving` state for download-dir moves

- **Status:** Accepted (2026-11-06)
- **Audit finding:** G9 · **Spec:** §16.4, §16.7

## Context
`PATCH /torrents/{id}` with `downloadDir` moves the data (§8.1). A move across filesystems can take minutes, and the spec defines no state for it, no conflict policy and no behavior during the move.

## Decision
- Add the state `moving` to §5.1. There is no network activity while moving.
- On the same filesystem, rename. Otherwise copy, fsync and delete the source, in the background. Then return to the desired state.
- If any destination file already exists, reject the move with `409 target_exists` and move nothing.
- The PATCH returns `202 Accepted` when a move starts. Torrents in `metadata` just update the path (`200`).
- Pause, recheck, move and remove during `moving` return `409 invalid_state`.

## Consequences
- The `state` enum gets a ninth value, which affects the API, SSE, UI badges, filters and tests.
- If the app is interrupted during a copy, the source files are still intact. On restart the torrent is still in its old location, and the move can be retried.
