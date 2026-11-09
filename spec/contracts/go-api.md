# Pinned Go APIs

**Owner:** architect role. Changes need an ADR and a matching change to the acceptance tests.

Acceptance checks are black-box wherever possible: they build `./cmd/torrentd` and talk to it over HTTP, the CLI and the filesystem. Implementers are free to design everything else. Where black-box testing isn't practical (pure parsers, fuzz targets), the acceptance tests call the **minimal APIs pinned here**. Implementers must provide these exact signatures. They may add anything else around them.

Acceptance tests are written *in package* (`package bencode`, not `bencode_test`), so the product module path is up to the implementer. `torrentd` is a sensible choice.

---

## Binary and CLI (`./cmd/torrentd`)

Black-box tests depend on the following:

| Contract | Detail |
| --- | --- |
| Build | `go build -o <tmp>/torrentd ./cmd/torrentd` from the repo root, `CGO_ENABLED=0` |
| Flags | `--addr`, `--data-dir`, `--peer-port`, `--open`, `--log-level`, `--dev` exactly as in §10. Unknown flags exit non-zero. |
| Env | `TORRENTD_ADDR`, `TORRENTD_DATA_DIR`, `TORRENTD_PEER_PORT`, `TORRENTD_LOG_LEVEL` |
| Default data dir | Linux: `$XDG_DATA_HOME/torrentd`, or `$HOME/.local/share/torrentd` when `XDG_DATA_HOME` is unset. macOS: `$HOME/Library/Application Support/torrentd`. Windows: `%APPDATA%\torrentd`. |
| Database | `<data-dir>/torrentd.db` |
| Logs | `log/slog` to **stderr**. The default handler is `slog.TextHandler`, so lines contain `level=WARN` etc. |
| Readiness | The process is ready when `GET /healthz` returns 200. Tests poll for up to 15 s. |
| Shutdown | SIGINT or SIGTERM leads to a graceful shutdown (§6.2) and exit code 0 within 10 s |
| Peer port in tests | Tests pass `--peer-port 0`, which means "pick any free port". In M1, before the engine exists, the flag only needs to be accepted. |
| Dev mode | `--dev` allows the origins `http://localhost:5173` and `http://127.0.0.1:5173`. |
| Dev mode, SPA | With `--dev`, `GET /` and other non-API paths return 404 `not_found` JSON. The SPA is served by Vite. *(Adopted from the M1 run.)* |
| Wrong method | A known route requested with an unsupported method returns 405 with code `invalid_request` and an `Allow` header. *(Adopted from the M1 run.)* |
| Host without port | A `Host` header without a port means port 80, so it's only allowed when the server listens on port 80. *(Adopted from the M1 run.)* |

## `tools/depcheck`

```
go run ./tools/depcheck [-graph FILE] [-modfile FILE] [DIR]
```

- With no flags, it checks the module in `DIR` (default `.`) using `go mod graph`.
- `-graph FILE` reads the `go mod graph` output from FILE instead of running Go, and `-modfile FILE` reads the root `go.mod` from FILE. Tests use both, so they can run offline.
- **Exit code** 0 means clean, 1 means violations (one per line on stdout, each naming the module), 2 means a usage error.
- **Allowlist.** Two rules, both required:
  1. Every **direct** requirement of the root (a `require` line in the root `go.mod` *without* `// indirect`) is `golang.org/x/*` or `modernc.org/*`.
  2. Every other module in the graph is reachable from those direct requirements. This allows their transitive dependencies, such as `github.com/dustin/go-humanize` via `modernc.org/sqlite`. It catches a module that is marked `// indirect` but that no allowed dependency actually needs.

  The pseudo-modules `go@…` and `toolchain@…` that `go mod graph` prints are ignored.
- **Denylist** (always a violation, at any depth): `github.com/anacrolix/*`, `github.com/cenkalti/rain`, `github.com/jackpal/bencode-go`, `github.com/zeebo/bencode`, `github.com/nictuku/dht`, `github.com/IncSW/go-bencode`, `github.com/marksamman/bencode`, `github.com/arvidn/*`, and any module path containing `bencode`, `torrent`, `/dht` or `magnet`. The root module is exempt, so it may be called `torrentd`. Denylist messages contain the word `denylisted`.

## `internal/bt/bencode`

```go
package bencode

// Marshal encodes v canonically: dict keys sorted, struct fields emitted in
// key order. Supported: string, []byte, all int/uint kinds, slices/arrays,
// map[string]T, structs (tags below), pointers, interfaces, RawMessage.
func Marshal(v any) ([]byte, error)

// Unmarshal decodes in lenient mode: accepts unsorted dict keys. Rejects
// malformed input and trailing data after the value.
func Unmarshal(data []byte, v any) error

// UnmarshalStrict decodes in strict mode (A1): additionally rejects
// non-canonical integers (leading zeros, "-0", empty), non-canonical string
// lengths (leading zeros), unsorted dict keys and duplicate dict keys.
func UnmarshalStrict(data []byte, v any) error

// RawMessage holds the exact encoded bytes of a value. Decoding stores the
// input bytes verbatim; Marshal writes them back unchanged.
type RawMessage []byte
```

- **Struct tags:** `bencode:"name"`, `bencode:"name,omitempty"`, `bencode:"-"`. Untagged exported fields use the field name.
- **Decoding into `any`:** integers become `int64`, strings `string`, lists `[]any`, dicts `map[string]any`.
- **Decoding a string** into either a `string` or a `[]byte` field works.
- **Limits:** more than 64 nested lists or dicts is an error (64 levels is fine). A string length longer than the remaining input is an error, raised *before* any allocation.
- **Errors:** errors are returned, never panics. Any non-nil error counts as a rejection; the tests don't check error types.

## `internal/bt/metainfo` — magnet links

```go
package metainfo

// Magnet is a parsed BEP 9 magnet URI.
type Magnet struct {
	InfoHash    [20]byte // from xt=urn:btih: (40 hex chars, any case, or 32 base32 chars)
	DisplayName string   // dn, URL-decoded; "" if absent
	Trackers    []string // every tr, URL-decoded, in order, duplicates removed
	Peers       []string // every x.pe as "host:port", in order
}

// ParseMagnet parses a magnet URI. It returns an error if the scheme is not
// "magnet", if there is no xt=urn:btih:, or if the hash is malformed.
func ParseMagnet(uri string) (*Magnet, error)
```

## Milestone 3 additions (ADR-0011)

| Contract | Detail |
| --- | --- |
| Add / get | `POST /api/v1/torrents` (`file`, `downloadDir`, `startPaused`), `GET /api/v1/torrents` and `GET /api/v1/torrents/{id}`, as in `spec/openapi.yaml`. At M3, `GET …/{id}` must satisfy the `Torrent` schema. |
| Download dir | Must be absolute. It is created (with parents) if missing. Files go to `<downloadDir>/<files[].path>`. |
| Lifecycle | add → `checking` (fast: nothing on disk yet) → `downloading` → `seeding` when every wanted piece is verified. `completedBytes` and `progress` grow as pieces verify. `wastedTotal` counts discarded bytes. |
| Peer port | `--peer-port N` binds TCP on all interfaces; 0 means any free port. The announced `port` is the bound port. |
| Tracker | HTTP(S) announce to the torrent's trackers (tier order, BEP 12). Each announce fails and is retried independently; there's no UDP yet. |
| Peers | Outgoing TCP connections to tracker peers. Plaintext BitTorrent only (no MSE, no uTP). |

Package contracts: in Approach C, each package's frozen interface lives in its `contract.go`, which only the architect may edit (ADR-0011).

## Milestone 4 additions (ADR-0016)

| Contract | Detail |
| --- | --- |
| Lifecycle | `POST /api/v1/torrents/{id}/pause`, `/resume` and `/stop-seeding` return 204, or 404/409 per G18 (§16.4). `GET …/{id}` reports `stopReason` and `uploadedTotal`. |
| Seeding from disk | Adding a torrent whose complete data is already in `downloadDir` verifies it (`checking`) and goes to `seeding`. |
| Incoming peers | The peer port accepts BEP 3 connections for active torrents and serves pieces per the choking algorithm (§4.1). Connections for stopped or paused torrents are refused or closed. |
| Persistence | Torrents and their state survive restarts on the same `--data-dir` (§6.1 step 4, §7, §16.3). |
| Tracker peers | Dict peer lists may contain IPv6 literals. Bans are per IP (G10). |
| Package contracts (A+) | `internal/bt/{piece,choke,storage,peer,session}/contract.go`, `internal/store/contract.go`, `internal/api/contract.go` (ADR-0017, 0018, 0019). Shared test helpers: `internal/bt/btest` (goroutine-leak check, in-memory `session.Store`). |

Torrent-file parsing is **not** pinned. It is tested black-box through `POST /api/v1/torrents/preview` (see `spec/openapi.yaml`).
