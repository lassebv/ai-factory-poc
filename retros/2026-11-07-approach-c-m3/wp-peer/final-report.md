The `peer` package is done and committed on `impl/m3-c-peer` in two commits. All contract tests and my own tests pass with `-race`, and coverage is 98.5% against the 80% floor.

## What I built
- **`wire.go`:**
  - Handshake read and write: the protocol-length byte is checked first, then the protocol text.
  - `exchangeHandshake`: if the context is cancelled, it sets a past deadline on the connection to abort the exchange. It then waits for that to finish, clears the deadline and returns an error wrapping `ctx.Err()`. No goroutine or deadline is left behind.
  - Message reading: length prefix, then the id byte, then the size check (bitfield limit or `MaxMessageLen`, plus exact payload sizes), and only then the payload. Oversized messages are never read.
  - Message writing and `newPeerID` (`-TD0100-` plus 12 bytes from the injected source).
- **`conn.go`:** a read goroutine, a write goroutine (batched and buffered) and a third **supervisor goroutine**. The supervisor owns the keep-alive and idle timers from `cfg.Clock` and does the shutdown: it waits for both I/O goroutines, closes `Done`, then calls `OnClose` once.
  - When a timer fires, it only triggers a check against the stored times of the last read and last write. If the deadline hasn't really passed, the timer is re-armed. So a stale timer can't cause a false timeout, and the read goroutine never touches a timer.
- **`peer_test.go`:** extra unit tests plus `FuzzDecodeMessage`, which checks that anything that decodes re-encodes to the same bytes.
- **`notes/peer.md`:** my decisions and interpretations. `stub.go` is deleted.

## Results
- `go test -race -count=5 ./internal/bt/peer/` passes.
- `go vet`, staticcheck, gofmt and `harness/checks/lint-clock.sh` are all clean.
- `harness/factory integrity --base 8ed1518… --role implementer`: ok (5 files changed).
- **One unexplained fuzz failure:** I ran the fuzzer for about 90 seconds in total with no crashes and no saved crash input. But one early 20-second run exited with a failure; I had cut its output off, and four later runs (15–40 s) passed. I couldn't reproduce it.

## Escalations and workarounds
None. The contract was clear and sufficient. The one judgement call is the third goroutine: the contract describes one read and one write goroutine. The timers can't live on the write goroutine, because it can block in a network write forever when the peer stops reading, and the idle timeout still has to fire then. All three goroutines exit before `Done` closes, and the contract's goroutine-leak checks pass.

## For the integrator
- After `Send`, callers must not change a message's byte slices (`Block`, `Bitfield`, `Payload`); they aren't copied.
- `Close` drops any queued messages that haven't been written yet.
- Messages that arrive after `Close` are not passed to `OnMessage`.
- `OnMessage` blocking is the back-pressure. `Done` (and so `OnClose`) waits until any `OnMessage` call in progress returns.
- A keep-alive is only queued when nothing else is waiting to be sent.
- When the remote closes the connection, `Err()` returns `peer: read: EOF`, which wraps `io.EOF`.
