# ADR-0008: NFRs are defined as concrete benchmark scenarios

- **Status:** Accepted (2026-11-06)
- **Audit finding:** A13 · **Spec:** §16.9

## Context
§11 gives targets ("idle CPU < 2%", "RSS < 200 MB", "saturates 100 Mbit/s") but no scenario. None of them can be a gate as written.

## Decision
| NFR | Scenario |
| --- | --- |
| Idle CPU < 2% | 200 torrents loaded (100 stopped, 100 seeding with no peers). Measured over 60 s after a 60 s warm-up. |
| RSS < 200 MB | Same as idle, plus a 10-minute run with 500 peer connections (`max_peers_global = 500`). |
| 100 Mbit/s | Loopback swarm through a shaped link (token-bucket proxy, 100 Mbit/s, 30 ms RTT). The leecher sustains ≥ 95% of link capacity. Plus one manual real-world run. |
| 50 MB/s loopback | Two `torrentd` instances, one seeding, unshaped loopback, a 2 GiB torrent. |
| SSE < 100 KB/s | 200 torrents, all active. Bytes on the `/events` stream averaged over 60 s. |

All scenarios run on one named CI runner class, so results are comparable between runs.

## Consequences
- NFRs become automated L7 checks (NFR-01…05) that run nightly and at milestones, not on every merge.
- The harness needs a shaped-link proxy and a resource sampler. Both live in `tools/`.
