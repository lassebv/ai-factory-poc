# Architecture Decision Records

ADRs record decisions that involve a real trade-off and were made by the human owner. Engineering defaults owned by the factory are recorded directly in `project-description.md` §16, with rationale in `spec/spec-audit.md`.

| ADR | Title | Audit ID | Status |
| --- | --- | --- | --- |
| [0001](0001-hybrid-torrents-v1-view.md) | Accept hybrid torrents through their v1 view | C2 | Accepted |
| [0002](0002-crash-recovery-trust-bitfield.md) | Trust the persisted bitfield after an unclean shutdown | C3 | Accepted |
| [0003](0003-limit-override-semantics.md) | `limit_override` suspends both seeding limits | C4 | Accepted |
| [0004](0004-missing-files-error.md) | All files missing → `error`, not a silent re-download | G8 | Accepted |
| [0005](0005-moving-state.md) | Add a `moving` state for download-dir moves | G9 | Accepted |
| [0006](0006-peer-ban-policy.md) | Peer ban policy: 3 strikes, 24 h expiry | G10 | Accepted |
| [0007](0007-private-magnets.md) | Private magnets may use DHT/PEX until metadata arrives | G15 | Accepted |
| [0008](0008-nfr-scenarios.md) | NFRs are defined as concrete benchmark scenarios | A13 | Accepted |
| [0009](0009-quality-gates.md) | Code-quality gates | A14 | Accepted |
| [0010](0010-schema-evolution.md) | Schema evolution before and after v1 | T7 | Accepted |
| [0011](0011-m3-download-api-and-frozen-contracts.md) | M3 pulls a minimal add/get API forward; `contract.go` files are frozen | §15.3 | Accepted |

**New ADRs:** number them sequentially, use the same template (Context / Decision / Consequences), and add a row here. The architect role writes them, and the human approves them if they change scope, risk or the API contract.
