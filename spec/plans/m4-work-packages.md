# M4 work packages (for the single implementer)

Build them in this order. WP1–WP5 depend only on frozen contracts and their own contract tests, so each can be finished and made green on its own. WP6 integrates them, and WP7 wires the binary.

| WP | Package(s) | Replace | Done when |
| --- | --- | --- | --- |
| 1 | `internal/bt/piece` | `m4_todo.go`; rework `picker.go`; update the M3 expectations in `piece_test.go` | `go test ./internal/bt/piece` (incl. `TestContractPickerCost`), coverage ≥ 80% |
| 2 | `internal/bt/choke` (new) | `stub.go` | `go test ./internal/bt/choke`, coverage ≥ 80% |
| 3 | `internal/bt/storage` | `stub_m4.go`; move the per-torrent semaphores to the shared `Pools`; streaming hash; `Sync`; `Stat`; `PlanResume` | storage contract tests, coverage ≥ 80% |
| 4 | `internal/bt/peer` | `stub_m4.go` (`AcceptHandshake`); `OnPieceSent` in `conn.go` | peer contract tests |
| 5 | `internal/store` | `stub_m4.go` | store contract tests (real SQLite) |
| 6 | `internal/bt/session` | `stub_m4.go`; actor rework per the session contract (seeding, choking, endgame/expiry, bans, data path, flush, restore, lifecycle) | `contract_test.go` + `m4_contract_test.go`, `-race`, coverage ≥ 80% |
| 7 | `internal/api`, `cmd/torrentd` | lifecycle handlers and routes; JSON `stopReason`/`checkingProgress`; `Store`/`PreviousClean` wiring; `MarkCleanShutdown` after `Close` | api contract tests, then `harness/factory gate --ms 4` |

The packages are independent because every cross-package dependency goes through a frozen interface:
- `session` → `piece.Picker`, `choke.Choker`/`Meter`, `storage.Storage`/`Pools`, `peer.Conn`, `session.Store`;
- `api` → `TorrentService`.

Fakes exist where a neighbour is needed (`btest.MemStore`, the fake tracker and seeder in the session tests, the fake `TorrentService` in the api tests). WP1–WP5 never import `session`.
