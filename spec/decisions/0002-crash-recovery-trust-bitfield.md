# ADR-0002: Trust the persisted bitfield after an unclean shutdown

- **Status:** Accepted (2026-11-06)
- **Audit finding:** C3 · **Spec:** §16.3

## Context
§4.1 says to do a full recheck when file sizes or mtimes don't match the last flush. After `kill -9`, the mtimes always differ, because writes continued after the last flush. Read literally, every crash would cause a full recheck, which contradicts the intent of §11 ("loses at most 10 s of progress").

## Decision
- Write a clean-shutdown marker (`clean_shutdown` setting) on graceful shutdown, and clear it at startup.
- Piece data **must be fsynced before** a bitfield containing those pieces is persisted.
- On startup: if the last shutdown was clean, check sizes and mtimes (§4.1). If it was unclean, trust the persisted bitfield and check sizes only. Pieces completed after the last flush are downloaded again.

## Consequences
- Recovery after a crash is fast, and at most 10 s of progress is lost.
- If files were modified outside `torrentd` while it was crashed, the change goes undetected until a forced recheck. We accept this as very unlikely.
- The fsync ordering is a correctness invariant. It is tested with a fault-injecting filesystem (RES-03).
