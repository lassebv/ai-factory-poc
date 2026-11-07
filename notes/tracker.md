# tracker work package (M3, Approach C)

Files: `internal/bt/tracker/{client,announcer,loop}.go` and `tracker_test.go`; `stub.go` deleted.

## Decisions and interpretations

- **Query encoding:** I build every parameter by hand, and every value goes through `percentEncode` (%XX for bytes outside `[A-Za-z0-9.-_~]`, upper-case hex). That covers key and trackerid too. An existing query in the announce URL is kept and ours is appended with `&`. A URL fragment is dropped. `key` is omitted when empty. `compact=1` and `numwant=50` go on every announce.
- **Schemes:** only http/https (case-insensitive) with a non-empty host. Anything else, udp:// included, is an error from the Client, so to the Announcer it is just a failing tracker.
- **Response decoding:** lenient `bencode.Unmarshal` into `map[string]bencode.RawMessage`, then each field is decoded on its own. A field with the wrong type is treated as absent rather than failing the whole response. The body must start with `d`. Bodies are capped at 4 MiB, and any non-200 status is an error.
  - A `failure reason` that isn't a string still yields a `*FailureError`.
  - interval and min interval: a value ≤ 0 counts as 0, and the value is clamped to 2^31-1 s. complete and incomplete: a missing or negative value gives -1.
  - Compact peers: a trailing partial entry is ignored and port 0 is dropped.
  - Dict peers: `ip` must parse with `netip.ParseAddr` and must not have a zone. Port must be in 1..65535. `peer id` is ignored. A list containing a non-dict element is ignored as a whole.
  - IPv4 peers come first, then peers6, in response order.
- **Announcer:** HTTP calls run without the lock held; the order is snapshotted per Announce. On success the URL is moved to the front of its tier (we find its current index, so concurrent reorders are safe). A tracker id is stored per URL when it's non-empty. A cancelled ctx stops trying further trackers. The returned error is `errors.Join` of `"<url>: <err>"`-wrapped errors.
- **Loop:**
  - One goroutine. A single `Clock.NewTimer` exists only inside `wait` and is stopped on exit.
  - `Completed()` sets an atomic flag and does a non-blocking send on a 1-buffered channel. The pending completed is sent right after any successful announce once started has succeeded, or it wakes an idle loop.
  - If Completed arrives while a failed *regular* announce is waiting for its retry, the completed announce replaces that retry immediately. If the pending retry is for `started`, Completed is held until started succeeds. If it is for `completed`, Completed is a no-op.
  - Backoff is 15 s × 2^(n-1), capped at 30 min, and resets on success. The wait after a success is max(interval, min interval), or max(30 min, min interval) when interval ≤ 0.
  - `Stop` (sync.Once):
    1. cancels the internal context;
    2. waits for the goroutine to exit;
    3. sends the stopped announce with the caller's ctx and returns its error.
    Later calls return nil. A stopped announce is sent even if started never succeeded, because the contract says "exactly one".
  - Nil `Stats`, `OnPeers`, `Clock` and `Logger` all get safe defaults. Failures and tracker warnings are logged at Warn.

## For the integrator

- `NewHTTPClient(nil)` uses a dedicated `http.Client` with a 30 s timeout, over `http.DefaultTransport`.
- `OnPeers` and `Stats` run on the loop goroutine. `OnPeers` is only called with a non-empty slice, and the slice is not reused.
- No escalations. The contract was sufficient.
