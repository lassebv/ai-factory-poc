# ADR-0013: Blocks are written as they arrive; pieces are verified from disk

- **Status:** Accepted (2026-11-07, architect, Approach C run on M3)
- **Spec:** §4.1 Storage and Hash verification, §4.5, D9, G21, C2 · **Acceptance:** STOR-01, -02, -04, -05, -06, -07

## Context

There are two ways to verify a piece:

- **A.** Buffer every block of a piece in memory, hash the buffer, then write it out.
- **B.** Write each block to disk as it arrives, then hash the piece by reading it back.

Option A never puts bad data on disk. But its memory use is (pieces in flight × piece length), and piece length can be up to 256 MiB (A2). Option B costs one extra read per piece. However, it uses the same code path that `checking` and recheck (M4) need anyway: hashing data that is already on disk.

§4.5 asks for a disk worker pool (4) and a hash pool (`max(1, GOMAXPROCS/2)`).

## Decision

- **Option B.** `storage.WriteBlock` writes immediately. `VerifyPiece` reads the piece back, with pad ranges read as zeros, and compares its SHA-1. `MarkVerified` records the result and renames `<file>.part` to `<file>` once every piece overlapping that file is verified.
- **Worker pools as bounded concurrency.** The pools are implemented inside `storage` as semaphores: at most `DiskWorkers` I/O operations and `HashWorkers` hashes run at once, and callers block until a slot is free. The torrent actor never calls storage itself. It runs each call in a short-lived goroutine, which posts the result back as an event.
- **File handles.** An LRU keeps at most 128 handles open, including during preallocation.
- **Hashing lives in `storage`,** next to the file mapping it needs for pad files and spanning pieces.

## Consequences

- A corrupted piece reaches the disk briefly and is overwritten on re-download. That's harmless, because the file keeps its `.part` name until its pieces verify.
- `storage` doesn't depend on `piece` or `session`, so it can be built and tested in isolation.
- The number of goroutines waiting on storage is bounded by the blocks in flight (at most 250 per peer).
