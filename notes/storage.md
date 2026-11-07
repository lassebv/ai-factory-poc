# storage (M3, Approach C) — implementer notes

Files: `internal/bt/storage/storage.go` (Open, layout, I/O, verify, rename,
Close), `handles.go` (ref-counted LRU of `*os.File`), `storage_test.go`.

## Decisions and interpretations
- **Validation first.** Every `File.Path` (pad files included) must be
  non-empty and `filepath.IsLocal(filepath.Join(Path...))`; this happens
  before `MkdirAll(dir)`, so a rejected torrent creates nothing. A relative
  dir, nil info or non-positive piece length is an error.
- **Existing data.** If `<path>` exists it is used in place (named final, so
  it is never renamed); this wins over a `<path>.part` that also exists. A
  shorter existing file is extended with `Truncate`; a longer one is left
  alone (never shortened). Reads past the end of a file that is shorter than
  expected return zeros.
- **Preallocation** opens, truncates and closes one file at a time (≤ 1
  descriptor), outside the LRU.
- **Mapping.** Binary search on file end offsets; zero-length files are
  skipped; pad ranges read as zeros and writes to them are dropped.
- **Bounded concurrency.** `DiskWorkers` semaphore around every
  Read/WriteBlock, every 1 MiB read chunk of VerifyPiece and every rename.
  `HashWorkers` semaphore around VerifyPiece (taken before disk slots;
  order is always hash → disk → mutex, so no deadlock). VerifyPiece streams
  in 1 MiB chunks, so 256 MiB pieces don't need a 256 MiB buffer.
- **Handle LRU.** One mutex + `sync.Cond`. `open` counts real descriptors,
  including stale ones still in use, and never exceeds MaxOpenFiles: an
  acquirer evicts the LRU idle handle or waits. Each I/O op holds at most one
  handle at a time, so waiters can't deadlock. Files are opened under the
  mutex (exact counting, no double opens).
- **Rename.** MarkVerified keeps a per-file counter of unverified overlapping
  pieces; at zero the file's cached handle is dropped (closed now, or by its
  last user if in use: "stale"), then `<path>.part` → `<path>` under the
  mutex, so later opens use the new name. Idempotent per piece. If a rename
  fails the error is returned and that file is not retried (M4 recheck
  territory).
- **Close** marks closed, wakes blocked acquirers (they get ErrClosed),
  waits for in-flight calls, then closes all handles. Idempotent.
- No goroutines, no time, no randomness.

## Escalations
None. The contract was sufficient.
