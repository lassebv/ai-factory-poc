**Milestone 1 (Skeleton)**, §15.1:

- `go.mod` at the repo root (module path `torrentd`, Go 1.25). The only dependency is `modernc.org/sqlite`, plus `golang.org/x/*` if needed.
- `cmd/torrentd`: flags and env vars (§10), the default data dir, slog to stderr, startup (§6.1 steps 1, 2 and 5) and graceful shutdown (§6.2, where it applies).
- `internal/config`, `internal/store` (SQLite, WAL, `busy_timeout`, `foreign_keys`, embedded migrations with the §7.1 schema plus the §16.6 additions), `internal/api` (server, Host/Origin middleware, `--dev` CORS, the error shape, `/healthz`, a 404 JSON for unknown `/api` paths, the SPA fallback) and `internal/clock` (T1).
- `webembed.go` with `//go:embed web/dist`, and a committed placeholder `web/dist/index.html` whose `<title>` contains "torrentd". No React app yet.
- `tools/depcheck`, exactly as specified in `spec/contracts/go-api.md`.
- A `Makefile` with `build`, `test` and `gate` targets.
- Unit tests for your own code, enough to meet the coverage floors.

**Out of scope:** the BitTorrent engine, every API endpoint other than `/healthz`, and the frontend.
