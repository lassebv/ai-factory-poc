**Milestone 3 (Single-peer download)**, §15.3 + ADR-0011. Milestones 1 and 2 are merged.

torrentd downloads a torrent from tracker peers over plain TCP, verifies every piece, writes the files correctly, and ends in `seeding`. Packages, per §4.4:

- `internal/bt/tracker`: HTTP/HTTPS announce (BEP 3/23).
  - Events `started`, `completed`, `stopped`.
  - Compact and dict peer lists, `interval` and `min interval`, `failure reason` and `warning message`, tracker id.
  - Parameters per D7 (`compact=1`, `numwant=50`, a `key` per session) and the peer ID per D13.
  - Tiers per BEP 12.
  - UDP trackers are **not** in scope (that's M5).
- `internal/bt/peer`: the BEP 3 handshake and core messages, and a connection with separate read and write goroutines (§4.5).
  - Keep-alives.
  - The message size limit: over 16 KiB + 13 bytes is a disconnect, except bitfield.
  - Robust against garbage input: never panic.
  - Extensions (BEP 6, 10 and 11) are **not** in scope.
- `internal/bt/piece`: bitfield, and a picker that can serve one or more peers.
  - Random-first / rarest-first can be simple at M3; M4 refines it.
  - Block planning in 16 KiB blocks, with request pipelining (5–250 outstanding, D1).
- `internal/bt/storage`: map pieces onto files (pieces span file boundaries).
  - Sparse preallocation as `<file>.part`, renamed when the file's pieces are all verified (G21).
  - BEP 47 pad files are never on disk.
  - SHA-1 verification.
  - A bounded handle LRU: at most 128 open files (D9).
  - Disk I/O and hashing run on worker pools (§4.5).
- `internal/bt/session`: one torrent actor per torrent (§4.5).
  - The lifecycle `checking` → `downloading` → `seeding`.
  - Announces: `started` on add, `completed` on finish, `stopped` on shutdown within 3 s (§6.2).
  - Bind `--peer-port` (0 means any free port).
  - Connect to tracker peers.
  - Counters: `completedBytes`, `downloadedTotal`, `wastedTotal`.
- `internal/api` and `cmd/torrentd`: `POST /api/v1/torrents`, `GET /api/v1/torrents` and `GET /api/v1/torrents/{id}`, exactly as in `spec/openapi.yaml` and `spec/contracts/go-api.md` (milestone 3 additions). Start the session at startup and shut it down on SIGTERM.

**Out of scope:**
- seeding behavior (choking, serving uploads), resume and persistence (M4);
- UDP trackers, DHT, PEX, LSD and magnets (M5);
- the remaining endpoints, SSE and limits (M6);
- the UI.

**Notes from harness validation:**
- Go's `ServeMux` panics if a method-less `/api/v1/torrents/preview` pattern (which already exists, for 405 handling) coexists with `GET /api/v1/torrents/{id}`. Plan the route table accordingly.
- The black-box tests use real loopback TCP, a fake tracker and scriptable fake peers. SP-01 downloads from a real libtorrent seeder in Docker, so Docker must be running.
