# ADR-0015: Torrent actor, published snapshots, and shutdown order at M3

- **Status:** Accepted (2026-11-07, architect, Approach C run on M3)
- **Spec:** §4.5, §5, §6.1, §6.2, §16.7 G16 · **Acceptance:** ADD-01, TRK-01, SP-01

## Context

§4.5 prescribes one actor goroutine per torrent. Peer goroutines must not touch torrent state. The HTTP layer polls `GET /api/v1/torrents/{id}` several times a second during downloads, and a request must never wait behind a busy actor.

§6.2 orders shutdown as: HTTP shutdown (up to 5 s), then `stopped` announces (3 s). But TRK-01 requires the `stopped` announce to arrive within about 3.5 s of SIGTERM.

## Decision

1. **The actor.** Each torrent has one actor goroutine with an event channel. It owns the picker, the peer table, the counters and the state.
   - Peer read goroutines (`peer.Config.OnMessage`), storage jobs and the tracker loop only send events to it.
   - The actor never blocks on I/O. Storage calls run in goroutines, and the results come back as events.
2. **Snapshots.** After every state change and every verified piece, the actor publishes an immutable `session.Status` through an atomic pointer. `Get` and `List` read those snapshots and never talk to the actor. A snapshot is consistent: `seeding` implies `CompletedBytes == SizeBytes`.
3. **Shutdown order** (an interpretation of §6.2 in favor of TRK-01). On SIGTERM, `cmd/torrentd` does two things at the same moment: it starts `Session.Close`, whose `stopped` announces run in parallel and are bounded by 3 s, and it starts the HTTP graceful shutdown. It waits for both, then writes the clean-shutdown marker and closes the DB. HTTP shutdown is usually immediate, so this matches §6.2 in practice, and the 3 s announce budget is measured from the signal.
4. **Peer port.** `--peer-port` is bound on all interfaces (0 picks any free port), and the bound port is what gets announced. If binding fails, the session still starts (G16), and the error is available from `ListenErr`. Incoming connections may be accepted and closed until M4.
5. **The API surface** is `api.TorrentService` (Add, List, Get). `session.Session` satisfies it. The route table in `internal/api/contract.go` avoids the ServeMux conflict between the method-less `/api/v1/torrents/preview` pattern and `GET /api/v1/torrents/{id}`.

## Consequences

- Polling the API never contends with the engine.
- The integrator owns `session`. Its contract test is an end-to-end, in-process download against loopback fakes, so integration bugs show up before the black-box gate.
- See `escalations/M3-shutdown-order.md` for the §6.2 interpretation.
