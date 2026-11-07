# ADR-0014: The tracker package owns the per-torrent announce loop

- **Status:** Accepted (2026-11-07, architect, Approach C run on M3)
- **Spec:** §4.1 HTTP trackers, §16.2 A4, §6.2, D7 · **Acceptance:** TRK-01, -02, -03

## Context

The announce schedule involves several rules: `started`, regular announces, `completed` and `stopped`; `interval` and `min interval`; retry with backoff; and BEP 12 tiers. It could live in the torrent actor, or behind an interface in `tracker`. If it lives in the actor, the integrator has to build and debug timing logic late in the run, when the parallel phase is already over.

## Decision

`tracker` exposes three layers:

- `Client`: a single HTTP announce.
- `Announcer`: BEP 12 tiers. It shuffles each tier once, promotes the tracker that responds, and remembers the tracker id for each URL.
- `Loop`: the per-torrent schedule, driven by the injected clock.

The behavior of `Loop`:

- It sends `started` immediately.
- On a failure, it retries the same event after 15 s, doubling the delay each time, up to 30 min.
- After a success, it waits `max(interval, min interval)` (30 min when `interval` is missing).
- `Completed()` sends `completed` as soon as `started` has succeeded.
- `Stop(ctx)` sends exactly one `stopped` announce, bounded by ctx.

The actor supplies the counters through a `Stats` callback and receives peers through `OnPeers`. Both run on the loop goroutine, so the actor publishes its counters atomically.

`numwant` is always 50, including on `stopped`. The `key` and the peer ID are chosen once per process, by the session.

## Consequences

- The tracker work package can be completed and tested in isolation with `clock.Fake`. The contract tests pin the timing.
- Manual reannounce (A8) and scrape (A10) arrive later, as additions to `Loop`.
- The loop keeps at most one clock timer, and only while it is idle. The tests rely on that.
