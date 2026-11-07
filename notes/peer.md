# peer package: implementation notes (M3, Approach C)

Files: `internal/bt/peer/wire.go` (handshake, message codec, peer id),
`conn.go` (Conn), `peer_test.go` (own tests + `FuzzDecodeMessage`).
`stub.go` removed.

## Decisions and interpretations

- **readMessage order**: length prefix → id byte → size check → payload. For
  a bitfield the limit is `maxBitfieldLen` (clamped to `MaxBitfieldLen` when
  ≤ 0 or larger); every other id is limited by `MaxMessageLen`. Exact payload
  sizes for fixed messages are also checked before reading the payload, so an
  invalid message never allocates more than it declares. EOF before the first
  byte is `io.EOF`; inside a message it is `io.ErrUnexpectedEOF`.
- Empty bitfield / unknown payloads decode to a non-nil empty slice; an empty
  piece block (payload of exactly 8 bytes) is valid.
- **writeMessage** does not validate field sizes (it is our own output); it
  writes header and block/payload as two writes to avoid copying 16 KiB
  blocks. The Conn wraps nc in a `bufio.Writer` and flushes per batch.
- **exchangeHandshake**: `context.AfterFunc` sets a past deadline on nc when
  ctx is done. If the AfterFunc ran, we wait for it, clear the deadline and
  return an error wrapping `ctx.Err()`. On success no deadline is touched and
  nc is left open. The handshake is written then read sequentially.
- **Conn goroutines**: a read goroutine and a write goroutine as the contract
  says, plus a third *supervisor* goroutine that owns the keep-alive and idle
  timers and finishes the shutdown (waits for both I/O goroutines, closes
  Done, calls OnClose). The timers can't live on the write goroutine because
  it may be blocked in `nc.Write` indefinitely (a peer that neither reads nor
  sends); the idle timeout must still fire. All three exit before Done.
- **Timers**: both timers are created in `newConn` from `cfg.Clock`. A timer
  firing only triggers a check against `lastRead` / `lastWrite` (clock times
  stored atomically); if the deadline hasn't really passed the timer is reset
  for the remainder. This avoids stale-fire races and means the read goroutine
  never touches a timer: it just stores `lastRead` before calling OnMessage.
  Keep-alives count as reads. A keep-alive is queued only when the queue is
  empty.
- **Close**: first reason wins (`nil` for `Close`). `closeWith` closes nc to
  unblock I/O, drops the pending queue and never blocks, so it is safe from
  OnMessage. Messages read after close are not delivered. Remote close gives
  `peer: read: EOF` (wraps `io.EOF`).
- **Send queue**: Send fails with `ErrSendQueueFull` (and closes the conn)
  when 1024 messages are already queued and not yet taken by the writer. The
  caller must not mutate a message's slices after Send.
- **newPeerID**: prefix + 12 bytes from three `rnd.Uint32()` calls
  (little-endian), fully binary (D13 says "random bytes").

## Results
- `go test -race ./internal/bt/peer/...` passes (repeated `-count=5`).
- Coverage 98.5%. `go vet`, staticcheck and `lint-clock` are clean.
- `FuzzDecodeMessage` was run locally for about 90 s with no crashes. One
  early run exited non-zero, but I lost its output and couldn't reproduce it.
  No crasher was saved.

No escalations: the contract was clear and sufficient.
