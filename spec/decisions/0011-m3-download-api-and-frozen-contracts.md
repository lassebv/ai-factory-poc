# ADR-0011: Milestone 3 pulls a minimal add/get API forward, and package contracts are frozen

- **Status:** Accepted (2026-11-07)
- **Spec:** §15.3, §8.1, §8.2 · **Acceptance:** ADD-01, TRK-*, WIRE-*, STOR-*, SP-01

## Context

Milestone 3 must "download a torrent from one seeder" (§15.3). To test that black-box, there has to be a way to start a download and watch it. The API that does this is officially milestone 6.

Milestone 3 is also the first milestone with several packages that interact (tracker, peer, storage, piece, session). It will be built with Approach C (contract-first fan-out), and that approach needs package interfaces that parallel agents can't drift from.

## Decision

1. **The API moves forward.** Milestone 3 implements these parts of `spec/openapi.yaml`:
   - `POST /api/v1/torrents` with a `file` (+ `downloadDir`, `startPaused`), returning 201, 400 or 409;
   - `GET /api/v1/torrents`;
   - `GET /api/v1/torrents/{id}`. At milestone 3 its response must satisfy the `Torrent` schema; the extra `TorrentDetail` fields arrive in milestone 6.

   Magnet links, the remaining endpoints and SSE stay in their milestones.
2. **Behavior at milestone 3:**
   - A completed torrent ends in `seeding`. Seeding *behavior* (choking, uploading) is milestone 4.
   - The peer port (`--peer-port`, where 0 means any free port) is bound. Incoming connections may be accepted and ignored.
   - No persistence across restarts yet; that's milestone 4.
3. **Frozen contracts.** Any file named `contract.go` or `contract_test.go` in the product tree belongs to the contracts area (owned by human and architect). In Approach C, the architect writes these first: package interfaces, shared types, and in-memory fakes for neighboring packages. Implementers code against them, can't edit them, and escalate if one needs to change.
4. **Where checks moved:**
   - STOR-03 (skip-file boundary pieces) moves to milestone 6, because it needs the file-priority API.
   - WIRE-03 (oversize requests) moves to milestone 4, because it needs seeding.
   - The peer-wire fuzz target becomes WIRE-06 in milestone 4.
   - Milestone 3 instead gets a black-box garbage-input check, WIRE-04.

## Consequences

- Milestone 3 is fully testable from outside the process: a fake tracker, a scriptable fake peer, and a real libtorrent seeder in Docker (SP-01, which needs Docker).
- The scope of milestone 6 shrinks a little.
- Parallel implementers can't break each other's assumptions without an architect-approved change, which the write policy enforces.
