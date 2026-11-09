# M4 report: swarm engine (Approach A, single agent)

Milestone 4 is not formally done: `harness/factory gate --upto 4` exits 1. Every check that can run passes (79/80). The one gap is AC-06, whose test package doesn't exist and can only be written by the verifier.

The other three items in the definition of done hold:
- `harness/factory integrity --base main --role architect` passes (32 changed files).
- `make build` produces `./torrentd`, and `go vet ./...` is clean.
- Everything is committed and the working tree is clean.

## Final `harness/factory gate --upto 4`
```
ID         MS  TIER STATUS   DETAIL
AC-06      4   L4   missing  package directory ./tools/swarm does not exist

Progress
  M1    18/18  pass (100.0%)   fail 0   missing 0   error 0   manual 0
  M2    21/21  pass (100.0%)   fail 0   missing 0   error 0   manual 0
  M3    15/15  pass (100.0%)   fail 0   missing 0   error 0   manual 0
  M4    25/26  pass ( 96.2%)   fail 0   missing 1   error 0   manual 0
  ALL   79/80  pass ( 98.8%)   fail 0   missing 1   error 0   manual 0
```
- SEED-02 (libtorrent in Docker), CHOKE-01, RACE-01 and every RES-* check pass.
- Coverage: session 87.0%, store 87.5%, piece 97.9% and storage 96.0%. Quality, clock-lint and depcheck checks are clean.

## Commits (on `impl/m4-approach-a`, not pushed)
- `4cf7f08` piece: the M4 picker (random-first, then rarest-first, priority classes, endgame, a block is only accepted from the peer it was requested from).
- `f2c6b56` The M4 engine:
  - process-wide disk and hash worker pools;
  - seeding and incoming connections;
  - the choker (4 slots by rate plus 1 optimistic, rotated every 30 s), request timeouts and snubbing;
  - strikes and bans, persisted;
  - lifecycle commands and their API routes;
  - persistence and fast resume (fingerprint check, recheck, "Files missing");
  - `Storage.Sync`, and the store implementation for torrents and bans.
- `6d2ffbc` Unit tests for session and store (an in-process two-session swarm plus actor-level tests); split `checking.go` out to stay under 800 lines.
- `fbfc0e0` ADR-0017, the AC-06 escalation, PROGRESS.md.
- `1ffa156` Fix for an RES-04 flake: stale or skipped requests no longer block the request pipeline.

## Escalation and interpretations
- **`escalations/M4-ac06-tools-swarm-missing.md`:** AC-06 points at `./tools/swarm`, which doesn't exist, and its test file would be verifier-owned. STOP-01 (`TestStopAndResumeSeeding`) already tests exactly this behavior and passes. I recommend re-pointing AC-06 at it.
- **ADR-0017** records every contract change:
  - piece: `Received` is strict and returns the peers to cancel; `Release` returns the released blocks; new `Discard`, `SetPriority`, `Endgame` and `HaveCount`;
  - storage: `Sync`;
  - session: `Store`/`Record`/`Ban`, `Load`/`Pause`/`Resume`/`StopSeeding`, new errors, `StopReason`;
  - api: the three action routes.
- **Main interpretations** (all in PROGRESS.md):
  - Adding a torrent triggers a full check only if one of its files already has data.
  - Paused and stopped torrents restored at startup don't open storage until resumed.
  - A fingerprint mismatch (or a size mismatch after an unclean shutdown) means a full recheck.
  - Seeding torrents don't dial out.
  - A request over 16 KiB disconnects the peer, since there's no fast extension yet.
  - A ban closes that IP's connections and discards in-progress pieces it contributed to.
  - At most 512 received blocks wait for the disk before requesting pauses.
  - A request stops counting toward the pipeline after 3 s, or as soon as the peer answers a later request (it was skipped).

## Where the spec or harness slowed me down
- **AC-06** is targeted at a package that doesn't exist, so `gate --upto 4` can't exit 0.
- **RES-04 hides a picker interaction.** The fake peer ignores requests for some pieces, and with random-first and rarest-first order that filled the request pipeline with dead requests. It passed when run alone and failed about half the time under load. It was a real robustness bug, now fixed.
- **Full gate runs were killed four times by the background time limit.** One stall happened while the host's clock jumped from 22:40 to 04:00, so the machine was most likely suspended. When the machine was responsive, the full gate took about 2.5 minutes.
- **The prompt mentions an in-repo goroutine-leak helper**, but I couldn't find one. The session contract test compares `runtime.NumGoroutine` instead.

## What I would change in these instructions
- Before a run, check that every entry for the milestone resolves to an existing test (for example with `gate --report`), so a `missing` like AC-06 is caught up front.
- Tell agents to run every new acceptance check several times under the full gate's load (for example `-count=10`), since that's how flakes like RES-04 show up.
- Mention that long gate runs may be killed by the tool environment, and suggest running per milestone.