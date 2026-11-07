I've implemented `internal/bt/storage`. All the frozen contract tests and my own unit tests pass with `-race`, coverage is 98.0%, and everything is committed on `impl/m3-c-storage`.

## What I built
I removed `stub.go` and added three files:
- **`storage.go`** holds the main logic:
  - **`open` checks before creating anything.** It rejects a nil info, a non-positive piece length and a relative `dir`. Every `File.Path`, pad files included, must be non-empty and pass `filepath.IsLocal`, otherwise `ErrUnsafePath`. No directory is made until all of these pass.
  - **Preparing files:** each non-pad file becomes a sparse `<path>.part` sized to its length, and zero-length files are created under their final name. Files are opened, sized and closed one at a time.
  - **Existing files:** an existing `<path>` is used in place and wins if a `.part` also exists. Existing files are extended if shorter and never shortened.
  - **Spanning ranges:** blocks are mapped onto files with a binary search over file end offsets. Pad ranges read as zeros and writes to them are dropped.
  - **Concurrency limits:** a `DiskWorkers` semaphore covers every block read or write, every 1 MiB read inside `VerifyPiece`, and every rename. A `HashWorkers` semaphore covers `VerifyPiece`. Reading in 1 MiB chunks means a 256 MiB piece doesn't need a 256 MiB buffer.
  - **Renames:** `MarkVerified` keeps a count of unverified pieces per file. When a file's count reaches zero, its cached handle is dropped and `.part` is renamed to the final name, so later reads use the new name. Calling it twice for the same piece is harmless.
  - **Shutdown:** `Close` wakes any blocked callers (they get `ErrClosed`), waits for calls in progress, then closes every handle. A second `Close` returns nil.
- **`handles.go`** is the cache of open files. It keeps a reference count per handle and caps the number of open files at `MaxOpenFiles` at every moment, counting handles that are still in use after a rename. A caller evicts the least recently used idle handle or waits for one. Each operation holds at most one handle at a time, so callers waiting for a slot can't deadlock each other.
- **`storage_test.go`** adds unit tests for:
  - invalid info and paths, and a `dir` that can't be created;
  - existing final or longer files;
  - files shrunk or deleted from outside;
  - write errors;
  - `ErrOutOfRange` and `ErrClosed` on every method;
  - a handle renamed while in use;
  - the open-file limit blocking until a handle is released;
  - `Close` while a caller is blocked;
  - concurrent use with pad files;
  - verifying a large piece in chunks.

My decisions are written up in `notes/storage.md`.

## Results
- `go test -race ./internal/bt/storage/` passes, and 10 repeated runs also passed, including the 300-file descriptor-limit test.
- Coverage is **98.0%**, against a floor of 80%.
- `go vet`, staticcheck 2025.1.1 and gofmt are clean.
- `harness/factory integrity --base 8ed1518… --role implementer` reports: integrity ok, 5 changed files.

## Escalations and workarounds
I wrote no escalations; the contract covered everything. Two interpretations go beyond its wording:
- **Both names on disk:** if `<path>` and `<path>.part` both exist, `<path>` is used.
- **Failed rename:** if the rename to the final name fails, `MarkVerified` returns the error and that file is not retried. A later recheck (M4) would have to handle it.

## For the integrator
- **Calls block.** Every method may wait for a disk, hash or open-file slot, so the torrent actor should call storage from short-lived goroutines, as ADR-0013 says.
- **Open-file count (STOR-06):** storage holds at most `MaxOpenFiles` handles, and holds none while `Open` runs or after `Close`. Nothing else in the session should hold handles under the download dir.
- **Progress:** `WantedBytes(i)` leaves out pad bytes, and its sum over all pieces equals `info.VisibleLength()`. It is the right amount to add to `CompletedBytes` after `MarkVerified`.
- **Re-adding a torrent:** `Open` treats every piece as unverified, even when files already exist. Re-checking existing data is M4 scope.
