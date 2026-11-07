# PROGRESS — milestone 1 (Skeleton), Approach A

## Status (2026-11-07)
`harness/factory gate --ms 1`: **17/18 pass**. The only failure is QUAL-01,
and it's caused by the M2 packages, not M1 code. See
`escalations/M1-qual-gates-include-m2-packages.md`.
`harness/factory integrity --base main --role implementer`: ok.
`make build` and `go vet ./...` are clean.

## Done
- `go.mod`: module `torrentd`, Go 1.25. The only direct dependency is
  `modernc.org/sqlite` v1.38.2 (the rest are indirect through it).
- `internal/config`: flags > env > defaults (§10) and the OS default data
  dir per the CLI contract. Unknown flags exit 2, `-h` exits 0.
- `internal/store`: WAL, `busy_timeout(5000)`, `foreign_keys(1)` via DSN
  pragmas. Embedded `migrations/0001_init.sql` (§7.1 plus §16.6:
  `global_stats`, `banned_peers.expires_at`), each migration in its own
  transaction and recorded in `schema_migrations`. Settings get/set/delete,
  and the clean-shutdown marker (C3): cleared on start, written last on
  graceful shutdown.
- `internal/clock`: the `Clock`/`Timer`/`Ticker` interfaces, `Real()`, and a
  `Fake` for deterministic tests (T1).
- `internal/api`: Host allowlist (G17) → `--dev` CORS → Origin check on
  POST/PUT/PATCH/DELETE → mux. Covers the `Error` shape, `/healthz`,
  `/api/*` 404 `not_found`, the SPA fallback (real files are served as-is,
  everything else gets index.html) and panic recovery to a 500 `internal`.
- `cmd/torrentd`: startup §6.1 steps 1, 2 and 5. Logs a WARN on non-loopback
  binds. `--open` is best-effort. SIGINT/SIGTERM → graceful HTTP shutdown
  (5 s) → marker → DB close → exit 0.
- `webembed.go` (package `torrentd`, `WebFS()`) and the placeholder
  `web/dist/index.html`.
- `tools/depcheck`, per the contract.
- `Makefile`: `build` (works without the frontend), `test`, `vet`,
  `depcheck`, `gate`.

## Decisions
- **M2 API stubs.** `internal/bt/bencode` and `internal/bt/metainfo` hold
  only the pinned signatures, which return `ErrNotImplemented`. Without them
  the verifier's M2 acceptance tests don't compile, and `go vet ./...` and
  staticcheck fail. M2 replaces the stubs.
- **Wrong method on non-API paths.** POST /healthz, DELETE /settings and the
  like return `405` with code `invalid_request`. ErrorCode has no
  `method_not_allowed`, and OpenAPI doesn't define 405 responses.
- **`--dev` GET /** returns a 404 `not_found` JSON (the SPA isn't served).
  `/healthz` and `/api` still work.
- **Host header without a port** is treated as port 80, so `localhost` is
  only allowed when the server listens on port 80.
- **Origins:** same-origin means `http://` plus an allowed Host entry. With
  `--dev`, `http://localhost:5173` and `http://127.0.0.1:5173` are added.
  `null` is rejected.
- **`--peer-port`:** accepted and validated (0–65535). `-1` internally
  means "use `listen_port` from settings". Unused until the engine exists.
- **Default settings (§7.2)** are not seeded yet. That belongs with
  `/settings` (M6).

## Next (not M1)
- M2: replace the stubs with the real bencode and magnet packages, and add
  `/api/v1/torrents/preview`.
