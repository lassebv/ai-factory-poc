# ADR-0020: Milestone 5 discovery: test hooks and externally observable behavior

- **Status:** Accepted (2026-11-09, human)
- **Spec:** §4.1 (UDP trackers, DHT, PEX, LSD, magnets, private), §6.1 step 3, §16.10 T3

## Context

M5 adds peer discovery. Testing it black-box needs hermetic test hooks (T3): there must be no public DHT and no real-network multicast in the gate. It also needs a few behaviors that can be observed from outside the process.

## Decision

1. **Startup flags.** The following flags, with environment variables of the same name prefixed `TORRENTD_`, are normal configuration, not test-only code:

   | Flag | Default | Meaning |
   | --- | --- | --- |
   | `--dht-bootstrap host:port[,host:port…]` | the D4 list | Replaces the DHT bootstrap nodes. An empty value means no bootstrap. |
   | `--lsd-addr ip:port` | `239.192.152.143:6771` | The LSD multicast group and port (BEP 14). The IPv6 group stays as in D6. |
   | `--lsd-interface name` | all multicast-capable interfaces | Restricts LSD to one interface (tests use `lo0`/`lo`). |

2. **UDP socket.** DHT and UDP trackers share one UDP socket per family, bound to the peer port (C5). UDP tracker requests are therefore sent **from** the peer port.
3. **Magnets.** `POST /api/v1/torrents` accepts `magnet` (§8.2).
   - The torrent appears in state `metadata` with `hasMetadata: false`.
   - It fetches the info dict over `ut_metadata` from peers found via `x.pe`, `tr`, DHT, PEX or LSD.
   - It verifies the info dict against the info-hash, then goes to `checking` → `downloading`.
   - `GET …/{id}` then shows the files.
4. **Extension protocol (BEP 10).** torrentd sets reserved bit 20 and sends an extended handshake with `m` (at least `ut_metadata` and `ut_pex`), `v`, `p`, `reqq=250`, and `metadata_size` when it's known.
5. **Private torrents** (BEP 27): no DHT, PEX or LSD traffic for their info-hash, and no `ut_pex` in their extended handshakes.
6. **Disabling DHT, PEX and LSD per process** is M6 (settings). At M5 they're on by default.

## Consequences

- The verifier can build fake DHT nodes, a fake UDP tracker, PEX-capable fake peers and an LSD listener on loopback, and validate them against libtorrent.
- The architect can rely on these hooks being part of the CLI contract.

## Addendum (2026-11-18, human): clarifications from the M5 harness validation

These come from the verifier's `harness/acceptance/discovery/VALIDATION.md` and are binding for M5.

7. **No per-IP limits in the DHT.** The routing table and lookups must accept many nodes that share an IP but use different ports. The tests run every fake node on 127.0.0.1.
8. **First announces happen at start.** A started torrent sends its first DHT `announce_peer` (D5) and its first LSD announce (D6) within 60 s of starting. After that, the periodic cadences in D5 and D6 apply.
9. **Discovery during `metadata`.** A magnet in state `metadata` uses the DHT to find peers, and accepts incoming peer connections. PEX and LSD are optional in that state (ADR-0007). If the metadata turns out to be private, G15 applies from that moment on.
10. **BEP 15 connection IDs** are cached and reused for up to 1 minute, then replaced.
11. **The extended handshake** also contains `yourip`, as §4.1 says. `p` is required on outgoing connections only.
12. **TorrentDetail at M5.** At M5 only `files` must be complete for a resolved magnet. The per-peer `source` field is M6 (SRC-01).

**Not adopted:** a configurable IPv6 LSD group. The cross-talk the verifier identified is harmless, because test torrents have unique names.
