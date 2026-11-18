# M5 discovery harness: validation report

Verifier, Approach V, 2026-11-09/10. The suite is `harness/acceptance/discovery`, the
fakes are in `harness/acceptance/internal/fakes` (`udptracker.go`, `dht.go`,
`extpeer.go`, `lsd.go`, plus BEP 48 scrape in `tracker.go`), and the oracle
checks are in `harness/acceptance/oraclecheck/discovery_test.go` and `fakesrv/`.

The suite was validated in three ways:

1. **Fakes against libtorrent** (oraclecheck). The fakes talk to real libtorrent 2.0.11.
2. **Checks against libtorrent** (the shim). `harness/oracle/ltshim.py` puts libtorrent
   behind torrentd's CLI and HTTP contract, and the whole suite runs against it inside the
   oracle container. When a mature independent client passes a check, the check can be
   passed and observes behavior correctly. When it fails, I looked at the reason to decide
   whether the test or libtorrent/the shim was at fault. Five test bugs were found and fixed
   this way (see "Harness bugs found").
3. **Checks against the current product** (M4, no discovery). Every check must fail for a
   discovery reason, or pass if the behavior already exists.

No throwaway torrentd reference implementation was built. The 13 checks that libtorrent
did not pass are therefore only reasoned about, not proven passable (table below).

## Hermeticity change for all suites (important)

From M5 on, torrentd starts DHT and LSD by default (ADR-0020 §6). Without countermeasures,
every M1–M4 test would bootstrap from the public D4 routers and multicast on the LAN.
`internal/app` now gives every instance the following, as **environment variables**
(a pre-M5 binary ignores them, unlike unknown flags):

- `TORRENTD_DHT_BOOTSTRAP` = a harness UDP socket that discards everything. This is not the
  empty string, because "empty" and "unset" are easy for an implementation to confuse.
- `TORRENTD_LSD_INTERFACE` = the loopback interface.
- `TORRENTD_LSD_ADDR` = `239.192.152.143:<a free port per instance>`.

Tests override these through `Options.Env` or flags. The fake HTTP tracker now answers
`/scrape` (BEP 48) instead of recording it as an announce. Without that, M5 scrapes would
break the announce-counting checks from M3 and M4 (an F2-style regression).

`HARNESS_TORRENTD_BIN` (in `internal/app`) swaps the binary under test. It is for harness
self-validation only; the gate never sets it.

## Oracle results (fakes against libtorrent 2.0.11, all PASS)

The fakes run inside the oracle container next to libtorrent, all on its loopback (`go test ./acceptance/oraclecheck/ -run 'Against'`):

| Check | What it shows |
| --- | --- |
| TestUDPTrackerAgainstLibtorrent | libtorrent connects, announces (event started, right port and info-hash) and scrapes the fake UDP tracker. It gets both peers and the scrape counts 7/3, and ignores the wrong-transaction-id decoy sent before every reply. |
| TestDHTAgainstLibtorrent | libtorrent bootstraps from the 20-node fake network and walks it to the holders, which are only reachable in many hops. It connects to the peer they return and sends announce_peer with a valid token. Probing libtorrent's DHT the way DHT-03/07 probe torrentd: ping, find_node and get_peers give well-formed answers, a forged token is rejected, and a valid announce is stored and returned. |
| TestMetadataPeerAgainstLibtorrent | libtorrent fetches 3 ut_metadata pieces (45 KB info dict) from the fake and downloads. It rejects the fake's corrupted metadata ("invalid metadata received"), so the corruption in MAG-05 is detectable by a correct client. |
| TestPEXAgainstLibtorrent | libtorrent connects to a seeder introduced by the fake's ut_pex and finishes. The fake decodes libtorrent's own ut_pex messages (added, added.f, dropped, plus IPv6 keys). |

Not oracle-validated: the LSD fake. libtorrent's LSD group and port are fixed and can't be
moved to a per-test port. The LSD fake is checked against BEP 14's text, and loopback
multicast with IP_MULTICAST_IF was verified to work on the gate machine (macOS lo0).

## Per-check results

M4 means the result against the current product (commit 53b0842 + setup). LT means the result against libtorrent through the shim.

| ID | Test | M4 result (why that's right) | LT |
| --- | --- | --- | --- |
| UDP-01 | TestUDPTrackerAnnounce | FAIL: no UDP packet at all ("udp:// not supported") | PASS |
| UDP-02 | TestUDPStoppedAnnounceSingleAttempt | FAIL: no started announce over UDP | PASS |
| UDP-03 | TestUDPRetransmitBackoff | FAIL: no connect requests | FAIL: libtorrent uses its own retry schedule, not 15·2ⁿ |
| UDP-04 | TestUDPConnectionIDExpiry | FAIL: no UDP announce | PASS |
| UDPMUX-01 | TestSharedUDPSocket | FAIL: no KRPC from torrentd | PASS |
| MT-01 | TestTierOrderShuffleAndPromotion | **PASS** (BEP 12 was in the M3 contract). Not vacuous: 3 distinct first trackers, and 6 of 8 torrents started on a failing tracker | FAIL: libtorrent clamps interval=10 to its 5-min minimum |
| MT-02 | TestNextTierAfterTierFails | **PASS** (already in M3) | PASS |
| SCR-01 | TestHTTPScrape | FAIL: no scrape (A10) | FAIL: libtorrent's HTTP scrape fails with an empty error (shim, unresolved) |
| SCR-02 | TestUDPScrape | FAIL: no UDP announce | PASS |
| SCR-03 | TestNoScrapeWhenNotDerivable | FAIL: the control torrent was not scraped. Without the control this would pass vacuously | FAIL: same as SCR-01 (control) |
| DHT-01 | TestDHTRoutingTableSplits | FAIL: no KRPC query to the bootstrap node | PASS |
| DHT-02 | TestDHTLookupFindsPeers | FAIL: never downloads (no DHT) | PASS (26 of 50 nodes queried) |
| DHT-03 | TestDHTTokens | FAIL: no answer to get_peers | PASS |
| DHT-04 | TestDHTPersistence | FAIL: the bootstrap node is never contacted | PASS |
| DHT-06 | TestDHTMalformedPackets | FAIL: no answer to ping | PASS |
| DHT-07 | TestDHTAnswersQueries | FAIL: no answer to ping | PASS |
| DHT-08 | TestDHTAnnouncePeer | FAIL: no announce_peer | PASS |
| DHT-09 | TestDHTBootstrapConfig | FAIL: `--dht-bootstrap` is an unknown flag (exit 2) | PASS |
| EXT-01 | TestExtendedHandshake | FAIL: reserved bytes all zero, no extended handshake | PASS |
| PEX-01 | TestPEXReceive | FAIL: no extended handshake | PASS |
| PEX-02 | TestPEXSend | FAIL: no extended handshake | FAIL: libtorrent sent no message dropping C within 150 s (see the limitations) |
| LSD-01 | TestLSDAnnounce | FAIL: `--lsd-addr` is an unknown flag | FAIL: the shim can't move libtorrent's LSD |
| LSD-02 | TestLSDReceive | FAIL: no connection to the LSD peer | FAIL: same as LSD-01 |
| PRIV-01 | TestPrivateTorrentNoDHTPEXLSD | FAIL: the DHT control fails | FAIL: the LSD control fails (no LSD in the shim) |
| PRIV-02 | TestPrivateMagnetLockdown | FAIL: magnet rejected with 400 | FAIL: the shim crashes reading the torrent during the metadata transition |
| MAG-03 | TestMagnetMetadataFetch | FAIL: magnet rejected with 400 invalid_request | PASS |
| MAG-04 | TestEarlyBitfield | FAIL: magnet rejected with 400 | FAIL: shim crash (same as PRIV-02) |
| MAG-05 | TestMagnetMetadataVerified | FAIL: magnet rejected with 400 | FAIL: libtorrent requests metadata from the peer advertising 10 MiB+1 (it is laxer than T6) |
| MAG-06 | TestTorrentFileCompletesMagnet | FAIL: magnet rejected with 400 | FAIL: the shim's set_metadata/add_tracker doesn't trigger an announce |
| AC-09 | TestDiscoverySources | FAIL: magnet rejected with 400 | FAIL: LSD peers not reached (shim) |
| SRC-01 (M6) | TestPeerSourcesReported | FAIL: magnet rejected with 400 | FAIL: LSD (shim) |

Totals: against M4, 28 FAIL and 2 PASS (MT-01, MT-02) of the 30 M5 entries. Against
libtorrent, 17 of 31 tests PASS. Each libtorrent failure is explained above by a known
libtorrent or shim divergence, not by a test defect.

## Harness bugs found by the libtorrent run (fixed before committing)

1. **EXT-01** required `p` on incoming connections. BEP 10 says the receiving side need
   not send it, and libtorrent doesn't. `p` is now optional on incoming connections but
   must be correct when present.
2. **PEX-02** required that a peer is never listed in its own ut_pex message. libtorrent
   broadcasts one message per torrent to every peer, so the recipient can appear in it, and
   BEP 11 doesn't forbid that. The check was removed.
3. **PEX-02** required flag 0x10 on peers torrentd dialed. libtorrent doesn't set it there,
   and the spec gives no basis for requiring it. The check was removed; the 0x02 (seed)
   flag is still checked.
4. **DHT-01** classified "far" nodes by membership in the fake set. A correct client also
   returns the probe node, which is legitimately far. Nodes are now classified by ID bit.
5. **DHT-06** pinged from the flooding IP after the flood. libtorrent rate-limits and blocks
   that IP for 5 minutes, which is a legitimate defense. Survival is now checked through
   /healthz and outbound DHT work, and the unknown-method check runs before the flood.
6. **Magnet URIs:** `x.pe` was percent-encoded. libtorrent doesn't decode it, and real
   magnets carry a literal `host:port`. The harness now sends the literal form.

Two problems were found in self-review before any run:

- **UDP-04** first stalled a seeder for 62 s. Anti-snubbing (60 s) could legitimately drop
  it, so a seeder now connects in at t+62 s instead.
- **DHT-01** first relied on an unspecified behavior: inserting nodes that only send
  queries. torrentd's own lookups now reach both groups.

## Retro lessons applied

- **No piece-order assumptions (F10/F14).** No check depends on which piece is fetched
  first. Stalls are by gate or by peer, never by piece index.
- **No unpassable placeholders (F13).** Every M5 entry has a real black-box test. AC-09
  now points at a test that exists. DHT-05 (IPv6) moved to M8. The `source` reporting half
  of AC-09 became SRC-01 (M6), because no contract makes TorrentDetail.peers available at M5.
- **Mechanism, not just outcome (PICK-02).** Where it can be observed, the mechanism is
  checked: retransmit gaps (UDP-03), connection-ID reuse and expiry (UDP-04), the
  multi-hop lookup (DHT-02), bucket splitting (DHT-01), the PEX rate limit (PEX-02),
  metadata pieces requested (MAG-03), and the early bitfield used on its first connection
  (MAG-04).
- **No next-milestone breakage (F2).** The hermetic env defaults and the scrape routing in
  the fake tracker protect M1–M4. `gate --upto 4` was re-run (see the end of this file).
- **No vacuous negatives.** PRIV-01, SCR-03 and DHT-09 each have a positive control in the
  same instance.

## Known limitations

- **Not observed black-box:**
  - token rotation every 5 min, including acceptance of the previous token (DHT-03);
  - the 30-min scrape period (only "exactly one in ~20 s" is checked);
  - the 15-min announce_peer and 5-min get_peers cadence (only "within 60 s of start");
  - the 50-entry PEX cap: at most 50 peers per torrent can be connected, so it can't be
    exceeded (only `≤ 50` is checked);
  - incoming LSD rate limiting (no rate is specified);
  - retransmission beyond n=1, and n ≤ 8.

  These are left to implementer unit tests and review.
- **k=8 is only partly checked.** DHT-01 checks splitting and `≤ 8` per response. A table
  with unbounded buckets would pass.
- **Assumptions tests rely on (proposed for the contract, below):**
  - the DHT accepts many nodes on 127.0.0.1 with different ports (no per-IP restriction);
  - an incoming peer connection is accepted for a magnet in `metadata` (MAG-05);
  - an LSD-announced peer is dialed at the packet's source IP.
- **Interpretations flagged for human review:**
  - DHT-08: the first announce_peer happens at torrent start, not after 15 min.
  - LSD-01: the first LSD announce happens at start.
  - UDP-04: reusing the connection ID within 1 minute is required.
  - PRIV-02: DHT is *required* during `metadata`, based on ADR-0020 §3 ("found via … DHT"),
    while PEX and LSD stay optional (ADR-0007 "may").
  - EXT-01: requires `yourip`. It is in §4.1 but not listed in ADR-0020 §4.
  - SCR-03: swarm counts come from the announce response when there is no scrape (D14).
- **Possible flakiness:**
  - PEX-02 keeps peers that are useless to each other connected for about 3 minutes. An
    implementation that drops mutually uninteresting peers quickly would fail it.
  - UDP-03's gap tolerances (14–20 s, 28–38 s) allow some jitter, but not a wildly
    different base interval.
- **Platform:** LSD tests need multicast on loopback. This was verified on macOS lo0. On
  Linux, `lo` may need `ip link set lo multicast on`.
- **IPv6 LSD cross-talk:** `[ff15::efc0:988f]:6771` can't be configured (ADR-0020 §1), so
  parallel instances may hear each other's IPv6 LSD announces on loopback. Test torrents
  have unique names, so this is harmless, but it isn't fully hermetic.
- **Shim results:** the LT column comes from one run of a best-effort shim. The shim
  segfaults intermittently under parallel load. Failures marked "shim" say nothing about
  the test.

## Proposed contract additions (for the architect/human; I can't edit contracts)

1. **ADR-0020 addendum:** the DHT must not limit routing-table or search entries per IP
   (tests run every fake node on 127.0.0.1).
2. **ADR-0020 addendum:** add `--lsd-addr6` (or let `--lsd-addr` disable the IPv6 group in
   tests), for full LSD hermeticity.
3. **Clarify D5 and D6:** the first DHT announce and the first LSD announce happen when the
   torrent starts.
4. **Clarify G15/ADR-0007:** DHT is used during `metadata` (implied by ADR-0020 §3); PEX
   and LSD are optional.
5. **Clarify BEP 15 connection-ID handling:** IDs are cached and reused for up to 1 minute.
6. **ADR-0020 §3/§4:** incoming connections are accepted for torrents in `metadata`, and
   `yourip` is part of the handshake (as §4.1 says).
7. **go-api.md:** state when GET /{id} must satisfy the full TorrentDetail schema (peers
   with `source`, for SRC-01). I assumed M6. At M5 only `files` is required, per ADR-0020 §3.
8. **API-03 (M6, package level) overlaps MAG-06 (M5, black-box).** Consider retiring API-03.

## Reproducing

```sh
cd harness
go test ./acceptance/discovery/ -count=1 -v          # against ./cmd/torrentd (~2 min on M4)
go test ./acceptance/oraclecheck/ -run Against -v    # fakes vs libtorrent (Docker)
# checks vs libtorrent (Docker):
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o /tmp/m5shim/disc.test ./acceptance/discovery/
docker run --rm -v "$PWD/..:/repo:ro" -v /tmp/m5shim:/t -e GATE_ROOT=/repo \
  -e HARNESS_TORRENTD_BIN=/repo/harness/oracle/ltshim.py torrentd-oracle:1 \
  /t/disc.test -test.v -test.parallel 6 -test.timeout 15m
```

## Gate status (2026-11-10)

- `harness/factory validate`: 166 entries, valid.
- `harness/factory gate --upto 4 --tier L1,L2,L3,L4` passed **74/74**, including RACE-01
  and every M1–M4 harness suite that uses the changed `internal/app` and `internal/fakes`
  (`runs/gate/20261110T062011Z.json`). The full `gate --upto 4` run (with the L0/L5/L6
  scripts and Playwright) was stopped at the 60-minute background limit before printing a
  summary. Those checks only look at product code, which this branch doesn't touch.
- `harness/factory gate --ms 5` did not finish on this machine. The host went to sleep
  during the runs (an 8-minute wall-clock gap inside a 70-second test; Go's monotonic clock
  stops while the host sleeps, so `-timeout` never fired). The same tests run directly with
  `go test ./acceptance/discovery/ -v` completed in 117 s (28 FAIL, 2 PASS: MT-01, MT-02,
  as in the table above). A second direct run reproduced every result except DHT-02, which
  the next suspend cut off. Re-run `gate --ms 5` on a machine that stays awake before the
  M5 implementation run.
