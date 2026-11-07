# ADR-0004: All files missing → `error`, not a silent re-download

- **Status:** Accepted (2026-11-06)
- **Audit finding:** G8 · **Spec:** §16.3

## Context
The spec covers "download dir missing" but not the case where the directory exists and the files are gone. A common cause is an unmounted external drive whose mount point still exists. A silent recheck would then download everything again, possibly onto the wrong disk.

## Decision
- If the persisted bitfield is non-empty and **all** of the torrent's files are missing, the torrent goes to `error` with the message "Files missing". The user can Retry, Recheck or Remove.
- If only **some** files are missing, recheck and download the missing data again.

## Consequences
- Large accidental re-downloads are prevented. The user must take an explicit action to recover.
- One more `error` cause that the UI and E2E tests must cover (RES-04).
