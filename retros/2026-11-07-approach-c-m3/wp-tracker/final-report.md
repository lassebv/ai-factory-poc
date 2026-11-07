The tracker package is done and committed: all ten contract tests pass with `-race`, coverage is 97.8%, and `go vet` and staticcheck report nothing for the package. Integrity against the base commit passes: "6 changed files, all writable by role implementer".

## What I built (`internal/bt/tracker`, `stub.go` deleted)
- **`client.go`, the HTTP client:**
  - Every query value is percent-encoded by hand, byte by byte. A query already in the announce URL is kept, and ours is appended with `&`.
  - `compact=1` and `numwant=50` go on every announce. `event` is left out for `EventNone`, and `trackerid` is left out when empty.
  - Only http and https URLs are accepted; `udp://` returns an error.
  - Responses are read leniently with the bencode package, one field at a time, so a field with the wrong type counts as missing instead of failing the whole response.
  - Peers can arrive in compact form, as a list of dicts, or as compact `peers6`. Peers with port 0, hostnames and zoned addresses are dropped. Missing seeder and leecher counts become -1.
  - A non-200 status, a body that isn't a bencoded dict, or a body over 4 MiB is an error.
- **`announcer.go`, the BEP 12 announcer:**
  - Each tier is shuffled once at construction.
  - Tiers are tried in order without holding the lock during HTTP calls.
  - The tracker that answers moves to the front of its tier, and its tracker id is remembered per URL.
  - When every tracker fails, the error joins all the individual errors.
- **`loop.go`, the announce loop:**
  - It runs on one goroutine and holds at most one timer from the injected clock, only while idle.
  - `Completed()` wakes an idle loop through a channel, not the timer.
  - Retries start at 15 s and double up to 30 min. After a success it waits the longer of `interval` and `min interval`.
  - `Stop(ctx)` cancels the loop, waits for its goroutine to exit, then sends one stopped announce bounded by ctx. It only does this once.
- **`tracker_test.go`:** my own tests for the URL encoder, URL building, malformed and lenient responses, HTTP errors, backoff and interval maths, and edge cases in the loop.
- **`notes/tracker.md`:** my decisions and interpretations.

## Escalations and contract workarounds
None. The contract was enough. Three places where I had to interpret it:
- If `Completed()` arrives while a failed regular announce is waiting to retry, the completed announce replaces that retry and goes out at once.
- If the retry pending is for `started`, the completed announce waits until `started` succeeds.
- A stopped announce is sent even if `started` never succeeded, because the contract says "exactly one".

## For the integrator
- `harness/checks/quality-check.sh` fails on the whole tree. The failures are staticcheck warnings in other packages' frozen files (`peer`, `piece`, `session` and `storage` contract and contract-test files), not in tracker. The architect will need to fix them before integration can pass.
- `NewHTTPClient(nil)` uses its own client with a 30 s timeout.
- `Stats` and `OnPeers` are called on the loop goroutine. `OnPeers` only gets non-empty slices, and they aren't reused afterwards, so it can pass them straight to the torrent actor.
- If `Stats`, `OnPeers`, `Clock` or `Logger` is nil, the loop uses a safe default.
