**Milestone 5 (Discovery)**, §15.5 + ADR-0020. Milestones 1–4 are merged.

What M5 adds:

- **UDP trackers (BEP 15):**
  - connect, announce and scrape with transaction IDs;
  - connection-ID expiry (re-connect after 1 min);
  - retransmission at `15·2ⁿ` s (D8);
  - DHT and UDP trackers share the socket bound to the peer port (C5).
- **Multi-tracker (BEP 12) and scrape (BEP 48), over HTTP and UDP.**
  - Tiers in order, shuffled within each tier, and a tracker that responds is promoted.
  - Scrape every 30 min, plus once after the first announce (A10).
  - `swarmSeeders` and `swarmLeechers` are reported (D14).
- **DHT (BEP 5):**
  - a Kademlia routing table (k=8, splitting) and KRPC `ping`, `find_node`, `get_peers` and `announce_peer`;
  - token rotation every 5 min;
  - bootstrap from `--dht-bootstrap`, otherwise D4;
  - the routing table and node IDs persist (G3, `dht_nodes`);
  - announcing per D5, using IPv4 at M5. IPv6 (`nodes6`, BEP 32) is M8.
- **Extension protocol (BEP 10)** and **PEX (BEP 11):**
  - the extended handshake per ADR-0020;
  - `ut_pex` sent and received, at most 1 message per minute per peer, at most 50 added and 50 dropped (D3), with flags honored;
  - IPv6 `added6` is M8.
- **LSD (BEP 14):** multicast announces on `--lsd-addr` / `--lsd-interface`, every 5 min per torrent, with incoming announces rate-limited.
- **Magnets (BEP 9):**
  - add via the `magnet` field;
  - fetch the metadata over `ut_metadata` (16 KiB pieces, `metadata_size` ≤ 10 MiB, T6), verify it against the info-hash, then continue as a normal torrent;
  - buffer a bitfield that arrives before the metadata (T6);
  - G14: a `.torrent` added for a pending magnet completes its metadata.
- **Private torrents (BEP 27, G15):**
  - only their own trackers;
  - a magnet that resolves to private disconnects its DHT, PEX and LSD peers and stops those sources.

**Out of scope:** IPv6 DHT, PEX and announces, and BEP 6 (M8); settings toggles for DHT, PEX and LSD, `/stats` with the DHT node count, and the remaining endpoints (M6); the UI (M7).
