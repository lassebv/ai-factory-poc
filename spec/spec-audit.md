# Spec Audit — `torrentd`

**Audited document:** `project-description.md`
**Date:** 2026-11-06
**Status:** **Accepted in full** by the human owner on 2026-11-06. The normative text is in `project-description.md` §16, and the ADRs for the human-owned items are in `spec/decisions/`. This document keeps the reasoning behind each decision.

## How to use this document

Each finding has an ID, a short description, a recommendation and an **owner**:

- **Human:** a product, scope or risk decision. Confirm the recommendation or override it.
- **Factory:** an engineering default. It is written down so that every agent makes the *same* choice instead of each guessing differently. Override it if you disagree.

Once the findings are resolved, the accepted decisions get folded back into the spec (as an amendment section or as ADRs in `spec/decisions/`). Each decision that can be tested gets an entry in `spec/acceptance.yaml`.

### Summary

| Category | Count | Owner: Human |
| --- | --- | --- |
| C — Contradictions (two parts of the spec disagree) | 7 | C2, C3, C4 |
| G — Gaps (behavior not specified) | 22 | G8, G9, G10, G15 |
| A — Ambiguities (more than one reasonable reading) | 16 | A13, A14 |
| T — Testability and harness implications | 7 | T7 |
| D — Engineering defaults (table) | 14 | — |

**The 10 Human decisions:** C2, C3, C4, G8, G9, G10, G15, A13, A14, T7.

---

## C — Contradictions

### C1. The SSE snapshot does not fit the bandwidth budget · Factory
- **Spec:** §8.3 says the `snapshot` carries `TorrentStats[]` (the §5.4 objects, about 30 fields each) every second. §11 says the snapshot for 200 torrents must stay under 100 KB/s.
- **Issue:** A §5.4 object serialized as JSON is about 800–1000 bytes. 200 of them come to about 160–200 KB/s, roughly twice the budget.
- **Options:** (a) A slim snapshot with only the fields that change (id, state, progress, completedBytes, rates, peers/seeds, eta, totals, ratio, seedingSeconds, checkingProgress), about 300 B each, so about 60 KB/s. Static fields come from REST and the `torrent.added`/`torrent.metadata` events. (b) Deltas containing only the torrents that changed, with a full snapshot as the first message on each connection. (c) gzip the SSE stream.
- **Recommendation:** (a). Add (b) or (c) later only if a measurement shows they are needed. This changes the API contract, so the OpenAPI document must state it.

### C2. Hybrid torrents are a non-goal, but interop needs them · **Human**
- **Spec:** §1.2 lists BEP 52 and hybrid torrents as a non-goal. §11 and §14.10 require interop with qBittorrent and Transmission in both directions.
- **Issue:** Recent qBittorrent builds (libtorrent 2.x) can create hybrid v1+v2 torrents, and many published torrents are now hybrid. Hybrid torrents use BEP 47 padding files. A client that rejects them or writes the pad files to disk fails on real-world input.
- **Options:** (a) Reject any torrent that has v2 fields. (b) Accept hybrid torrents through their v1 view only: v1 info-hash, v1 `pieces`, ignore `meta version`/`file tree`, and support BEP 47 pad files (never written to disk, excluded from sizes, treated as zeros when hashing). Reject v2-only torrents with `invalid_torrent`. (c) Keep hybrid out of scope and require v1-only interop fixtures.
- **Recommendation:** (b). It is a small amount of work with a large gain in interop. Full v2 support stays a non-goal.

### C3. Fast resume after a crash triggers a full recheck · **Human**
- **Spec:** §4.1 says to do a full recheck when file sizes or mtimes don't match the fingerprint from the last flush. §11 says `kill -9` "loses at most 10 s of … progress (pieces are re-verified via fast resume)".
- **Issue:** After a crash, the mtimes *will* differ from the last flush, because writes continued after it. A literal reading of §4.1 means every crash causes a full recheck. For a 100 GB torrent that takes many minutes, which contradicts the intent of §11.
- **Options:** (a) Accept a full recheck after every crash. (b) Write a clean-shutdown marker. On an **unclean** start, trust the persisted bitfield and check sizes only. This is safe if piece data is fsynced *before* the bitfield that includes it is written. Pieces completed after the last flush are downloaded again (≤ 10 s of work). (c) Same as (b), and also hash-check pieces that are not in the bitfield to recover the lost ≤ 10 s.
- **Recommendation:** (b). The trade-off is that changes made to the files outside `torrentd` *during* a crash window go undetected, which is very unlikely. A user can always force a recheck.

### C4. One `limit_override` flag, but "the limit that triggered it" · **Human**
- **Spec:** §5.2 says that after an automatic stop, "the limit that triggered it no longer applies until the user changes it". §7.1 has a single boolean column, `limit_override`.
- **Issue:** If a torrent stopped by its ratio limit is resumed, does its *time* limit still apply? And which change by the user clears the override: the per-torrent limit, the global limit, or either?
- **Recommendation:** The single flag suspends **both** seeding limits for that torrent. It is cleared when the user PATCHes either of *that torrent's* limit fields. Changing a global limit does **not** clear it. Otherwise, editing a global setting would suddenly stop torrents the user had explicitly resumed.

### C5. A single UDP socket, but IPv6 DHT needs its own · Factory
- **Spec:** §6.1 refers to "the UDP socket", singular, shared by DHT and UDP trackers. §4.1 requires IPv6 DHT (BEP 32).
- **Issue:** BEP 32 requires a separate routing table for each address family. Dual-stack socket behavior also varies by OS.
- **Recommendation:** Use one UDP socket per address family (`udp4` and `udp6`). Each is shared by DHT and the trackers, demultiplexed by first byte (`d` means KRPC, anything else is a tracker reply looked up by transaction ID). Keep two routing tables. `dht_nodes.addr` already holds either family.

### C6. Rechecking a `stopped` torrent that turns out to be incomplete · Factory
- **Spec:** §5.1 defines `stopped` as "Complete, and seeding was stopped". §5.3 allows a forced recheck from `stopped`.
- **Issue:** If the recheck finds missing pieces, the torrent can no longer be `stopped`.
- **Recommendation:** Move it to `paused` (`desired_state=paused`, `stop_reason` cleared, `limit_override` cleared). Never start network activity the user did not ask for.

### C7. The default peer limit is lower than the scale target · Factory
- **Spec:** The default `max_peers_global` is 200 (§7.2). §11 requires handling 500 total peer connections.
- **Recommendation:** These don't really conflict. The NFR scenario raises `max_peers_global` to 500, and the scale benchmark in the harness must say so explicitly.

---

## G — Gaps

### G1. There is no "retry" endpoint for `error` · Factory
- §5.3 has `error ──(user "retry")──▶ checking`, but no endpoint for it exists.
- **Recommendation:** `POST /resume` on a torrent in `error` means retry, which moves it to `checking`. The UI labels the button "Retry".

### G2. The half-open limit has no setting · Factory
- §4.1 has a default of 20 and §9.6 shows it in the UI, but §7.2 has no key for it.
- **Recommendation:** Add `max_half_open` (default 20) to the default settings.

### G3. The DHT node ID is not persisted · Factory
- A routing table is meaningless without the node ID it was built around, because buckets are XOR distances from your own ID. The spec persists the table but not the ID.
- **Recommendation:** Store `dht_node_id_v4` and `dht_node_id_v6` as internal settings keys that are not exposed by `GET /settings`. If an ID is missing or regenerated, discard the stored table.

### G4. Global lifetime totals have no storage · Factory
- `/stats` returns "lifetime totals". Summing the current torrents gives a total that decreases whenever a torrent is removed.
- **Recommendation:** Add a `global_stats` table with persisted counters, flushed alongside the per-torrent totals. This depends on T7.

### G5. The UI is unspecified for 5 of the 8 states · Factory
- §9.4 specifies only downloading, seeding and stopped.
- **Recommendation:**

| State | Progress | ↓ / ↑ | Peers | ETA | Row actions (plus the "All states" actions) |
| --- | --- | --- | --- | --- | --- |
| `paused` | bar with % | — | — | — | Resume |
| `queued`, incomplete | bar with % | — | — | — | Pause |
| `queued`, complete | full bar | — | — | — | Stop seeding |
| `checking` | bar showing `checkingProgress`, "Checking 42%" | — | — | — | Pause |
| `metadata` | indeterminate bar, "Fetching metadata" | speeds | peers | — | Pause |
| `error` | bar with % | — | — | — | Retry, with `errorMessage` shown inline or on hover |

### G6. There is no field for recheck progress · Factory
- **Recommendation:** Add `checkingProgress` (0–1, `null` unless the state is `checking`) to §5.4.

### G7. Raw and effective limits are not distinguished · Factory
- §5.4 exposes only the *effective* limits. The Options tab (§9.5) needs the *raw* per-torrent value to show "use global" or "unlimited".
- **Recommendation:** Add `ratioLimitSetting` and `seedTimeLimitSetting` to the detail object (`null` = use global, `-1` = unlimited, or a number), plus `maxPeers`, `limitOverride`, `queuePosition`, `pieceLength`, `pieceCount` and `magnetUri` (generated for torrents added from a file).

### G8. Behavior when files are missing at startup · **Human**
- §5.1 covers "download dir missing". It does not cover a directory that exists but whose files were deleted, or an unmounted external drive whose mount point is still there.
- **Options:** (a) Recheck and silently download everything again. (b) Go to `error` "Files missing".
- **Recommendation:** If the bitfield is non-empty and **all** of the torrent's files are missing, go to `error` with the message "Files missing" (the user can Retry, Recheck or Remove). If only **some** files are missing, recheck and download the missing data again. This avoids silently re-downloading 100 GB onto the wrong disk.

### G9. Moving data (`PATCH downloadDir`) is unspecified · **Human**
- There is no state for a move in progress, no policy for files that already exist at the target, and no behavior for moves across filesystems.
- **Recommendation:** Add a `moving` state. Network activity stops. If the target is on the same filesystem, rename. Otherwise copy, fsync, then delete the source, in the background. The torrent then returns to its desired state. Reject with `409 target_exists` if any destination file already exists. A torrent in `metadata` just updates its path.

### G10. The peer-ban policy is unspecified · **Human**
- §4.1 says to ban peers that "repeatedly" send bad data. There is no threshold, no rule for which peer gets blamed when several contributed blocks to a failed piece, no expiry, and no way to see or remove bans.
- **Recommendation:** Record which peers contributed blocks to each piece. When a piece fails its hash check, every contributor gets a strike. Ban an IP at 3 strikes. Bans are stored in `banned_peers` and **expire after 24 h** (add an `expires_at` column). There is no UI in v1, but `/stats` reports the banned count.

### G11. Queueing semantics are unspecified · Factory
- **Recommendation:**
  - Download slots are used by torrents in `downloading` and in `metadata`.
  - Stalled torrents still use a slot. There is no "slow torrent" exemption in v1.
  - One global `queue_position` order applies separately to the download class and the seed class.
  - At most 1 torrent is hash-checked at a time to avoid disk thrashing. Others wait in `checking` with `checkingProgress = 0`, and the UI shows "Waiting to check".
  - A torrent that finishes checking or downloading moves to `seeding` through the seed queue. It does not bypass the queue, despite the arrow in the §5.3 diagram.

### G12. The error-code list is incomplete · Factory
- **Recommendation:** Add `not_found`, `invalid_request`, `invalid_state`, `not_complete`, `already_exists`, `invalid_torrent`, `invalid_magnet`, `invalid_download_dir`, `target_exists`, `port_in_use`, `payload_too_large`, `forbidden_origin`, `no_metadata` and `internal`.

### G13. The bulk response is unspecified · Factory
- **Recommendation:** `200 {"results":[{"id":"…","ok":true}|{"id":"…","ok":false,"error":{…}}]}`. A failure on one ID does not stop the others.

### G14. Adding a `.torrent` for a magnet that is still fetching metadata · Factory
- **Recommendation:** Verify the file's info dict against the info-hash, use it as the metadata (the torrent moves to `checking`), merge the trackers, and respond `409 already_exists` with the `id`. The toast says "Already added — metadata supplied".

### G15. Private torrents added from a magnet · **Human**
- Whether a magnet is private is unknown until the metadata arrives. Until then, DHT and PEX would be used, which leaks the info-hash outside the private tracker.
- **Options:** (a) Accept the leak, as libtorrent does. Once the metadata shows `private=1`, disconnect peers whose source is not a tracker and stop DHT, PEX and LSD for that torrent. (b) Use only `tr=` trackers and `x.pe` peers for *every* magnet until the metadata arrives. This is safer but makes magnets without trackers unusable.
- **Recommendation:** (a).

### G16. Peer port precedence and busy ports · Factory
- **Recommendation:**
  - `--peer-port` overrides `listen_port` for that process only and is not persisted. The Settings page then shows the port read-only, with the note "set by --peer-port".
  - If the port is busy **at startup**, the HTTP UI starts anyway, the footer shows the error, and the user can change the port.
  - If the port is busy **when changed via PATCH**, the old binding is kept, the setting is not saved, and the response is `409 port_in_use`.

### G17. CSRF and DNS rebinding · Factory
- An `Origin` check alone does not stop DNS-rebinding *reads* such as `GET /torrents` or `/fs/validate`.
- **Recommendation:**
  - Check the `Host` header against an allowlist on **every** request: `localhost`, `127.0.0.1`, `[::1]`, and the host of `--addr`, each with the configured port.
  - On state-changing requests, an `Origin` header must be on the allowlist if it is present. A missing Origin is allowed, for curl and scripts.
  - `--dev` adds the Vite dev origin to the allowlist.

### G18. Which actions are valid in which states · Factory
- **Recommendation:**
  - `pause` on a complete torrent returns `409 invalid_state`. Use stop-seeding instead.
  - `stop-seeding` on an incomplete torrent returns `409 not_complete`, as the spec says.
  - `resume` on a torrent that is already active returns `204` and does nothing.
  - `recheck` in `metadata` returns `409 no_metadata`.
  - `PATCH /files` in `metadata` returns `409 no_metadata`.
  - Pause is allowed in `metadata`, `checking`, `queued` and `downloading`.

### G19. Fields before metadata arrives · Factory
- **Recommendation:** Add `hasMetadata: bool`. Until it is true, `sizeBytes` and `completedBytes` are `null`, and `files[]` and `pieces` are empty. `seedsConnected` counts peers that sent `have_all` or a full bitfield, which can only be judged once the piece count is known, so it is `0` until then.

### G20. Payload shapes are undefined · Factory
- The spec does not define the shapes of `files[]`, `/stats`, `/history`, the `/preview` input (multipart `file`) or `snapshot.global`.
- **Recommendation:** The architect role writes `spec/openapi.yaml` as the **first contract**, before any backend or frontend work starts. You review it, since it is the boundary between the frontend and backend work.

### G21. Naming and timing of part files · Factory
- §4.1 says to keep "`.part` until complete". It doesn't say whether that means until the file or the torrent is complete.
- **Recommendation:**
  - Each file is named `<file>.part` until *that file's* pieces are all verified, then renamed.
  - Data for boundary pieces (§4.3) goes in `<download_dir>/.torrentd-<infohash>.parts`.
  - When a file changes from skip to wanted, its bytes are copied out of `.parts` into the file.
  - Toggling `use_part_files` only affects files that are not yet complete.
  - `deleteData` also removes `.part` and `.parts` files and any empty directories the torrent created.

### G22. `completed` announce and `completedAt` · Factory
- **Recommendation:** `completedAt` is set the first time the torrent reaches 100%, including when the initial check finds it already complete. The `completed` tracker event is sent only when the completion happened through downloading in the current session (BEP 3).

---

## A — Ambiguities

### A1. How strict bencode parsing is · Factory
- §4.1 says the parser is "strict … when validating the info dict". Applying strictness everywhere would break on sloppy trackers and DHT nodes.
- **Recommendation:** Support two modes. **Strict** (canonical integers, sorted unique keys) for the info dict and the `ut_metadata` payload. **Lenient** (accept unsorted keys, still reject malformed input) for tracker, DHT and extension messages. Both modes enforce size and depth limits.

### A2. What counts as a valid piece length · Factory
- **Recommendation:** Require 16 KiB ≤ length ≤ 256 MiB. A power of two is not required, but a non-power-of-two length is logged. `len(pieces)` must be a multiple of 20 and must equal `20 × ceil(total / pieceLength)`.

### A3. Path safety beyond the spec's rules · Factory
- **Recommendation:**
  - Reject `..`, absolute paths and empty components, as the spec says.
  - Also reject duplicate paths after case-folding and normalization.
  - **Sanitize** names that are invalid on Windows (reserved names like `CON`, the characters `<>:"|?*`, trailing dots and spaces) by replacing them with `_` on *all* platforms, so the result is the same everywhere.
  - Prefer `path.utf-8` and `name.utf-8` when present.

### A4. Multi-tracker behavior · Factory
- qBittorrent's default announces to all trackers in all tiers. The spec describes strict BEP 12.
- **Recommendation:** Follow the spec and use strict BEP 12: try the tiers in order, shuffle within each tier, and promote a tracker that responds. User-added trackers go in the tier the user specifies.

### A5. "Strict priority" and when endgame starts · Factory
- **Recommendation:**
  - A piece's priority is the maximum priority among the wanted files it overlaps.
  - The picker works strictly by priority class (`high` > `normal` > `low`), with rarest-first within each class.
  - "Random until 4 pieces are complete" applies within the highest available class.
  - Endgame starts when every remaining wanted block has been requested at least once.

### A6. Which counters the seeding limits use · Factory
- **Recommendation:**
  - The ratio uses the §5.4 formula, which is defined as `0` when the denominator is 0.
  - The time limit uses the cumulative `seedingSeconds` across restarts.
  - Limits are checked only in `seeding`. A torrent that is already over its ratio when it completes stops right away, which is expected.

### A7. What bandwidth limits count · Factory
- **Recommendation:** Limits apply to all peer-wire bytes, both payload and protocol overhead. Displayed rates stay payload-only, as §5.4 requires. Tracker, DHT and LSD traffic are not limited.

### A8. Reannounce when `min interval` has not passed · Factory
- **Recommendation:** Return `204` and schedule the announce for the earliest allowed time. The tracker row shows the scheduled `nextAnnounce`.

### A9. PATCH semantics · Factory
- **Recommendation:** Use JSON Merge Patch (RFC 7386). A missing field is unchanged. For limits, `null` means "use global" and `-1` means "unlimited". Unknown fields return `400 invalid_request`.

### A10. Scrape frequency and HTTP scrape URL · Factory
- **Recommendation:** Scrape every 30 min, and once right after the first successful announce. Derive the HTTP scrape URL using the BEP 48 convention: replace `announce` with `scrape` in the last path segment. If that isn't possible, don't scrape over HTTP.

### A11. `/fs/validate` for a path that doesn't exist · Factory
- **Recommendation:** `exists` reports whether the path exists. `writable` means the path can be written, or, if it doesn't exist, that it can be created in its nearest existing ancestor. `freeBytes` comes from that same directory. A non-absolute path returns `400 invalid_download_dir`.

### A12. How transfer samples are computed · Factory
- **Recommendation:** Each 10 s sample is the *average* rate over the window, not the EMA. Skip rows where both rates are 0, and have the chart treat a gap as 0. Storing 200 torrents × 8640 rows per day is otherwise wasteful.

### A13. What the NFRs actually mean · **Human**
- "Idle CPU < 2%", "RSS < 200 MB" and "saturates 100 Mbit/s" need a defined scenario before they can be gates.
- **Recommendation:**
  - **Idle:** 200 torrents loaded (half stopped, half seeding with no peers), measured over 60 s after a 60 s warm-up.
  - **RSS:** the same scenario, plus a 10-minute run with 500 connections.
  - **100 Mbit/s:** verified on loopback through a shaped link (a token-bucket proxy, 30 ms RTT), plus one manual run against a real-world torrent.
  - All of these run on a named CI runner class.

### A14. What "readable, well-tested" means as a gate · **Human**
- **Recommendation:**
  - Coverage of at least 80% per `internal/bt/*` package, and at least 60% elsewhere.
  - Every exported identifier has a doc comment.
  - `go vet` and staticcheck are clean.
  - No file over about 800 lines.
  - A reviewer agent scores each change against `CLAUDE.md`, but its score only gives feedback and does not block a merge.

### A15. Formatting edge cases · Factory
- **Recommendation:**
  - Sizes under 1 KB show as `512 B`, with no decimal. Add `TB` above that.
  - Two-unit durations go up to days (`3d 4h`). An ETA over 365 days shows as `∞`.
  - "Inactive" means the state is not `downloading`, `seeding` or `metadata`.

### A16. The global paste listener · Factory
- **Recommendation:** Ignore paste events whose target is an input, textarea or contenteditable element. Open the dialog only when the pasted text matches `^magnet:\?`.

---

## T — Testability and harness implications

These need decisions before any code is written, because they shape the interfaces.

### T1. Inject a clock everywhere · Factory
- The spec is full of timers: the 10 s choke round, 30 s optimistic unchoke, 60 s snub, 1 min PEX, 5 min token rotation, the `15·2ⁿ` UDP backoff (up to about 64 min), minute-based seeding limits, and 24 h retention. Testing these in real time is either slow or flaky.
- **Recommendation:** Put an `internal/clock` interface in the shared kernel. Ban direct `time.Now`, `time.After` and `time.NewTicker` in `internal/bt/**`, `internal/stats` and `internal/store`, and enforce the ban with a lint check in L0.

### T2. Seed all randomness · Factory
- The peer ID, the random first pieces, the tier shuffle, DHT IDs and transaction IDs all use randomness.
- **Recommendation:** Inject a `rand.Source` so that tests are deterministic.

### T3. Keep the gate network hermetic · Factory
- **Recommendation:**
  - Tiers L0–L4 use loopback only.
  - The DHT bootstrap list, tracker URLs and LSD multicast group and interface are all configurable.
  - CI containers often have no multicast, so LSD tests use an injectable transport, plus one optional real-multicast test.

### T4. Interop container configuration · Factory
- qBittorrent and Transmission prefer encryption and uTP by default. Both are stretch goals here.
- **Recommendation:** Configure the interop containers to allow plaintext TCP. Generate fixtures as v1 torrents, and also hybrid ones if C2 is accepted.

### T5. Developer tooling and depcheck · Factory
- **Recommendation:**
  - Run linters and code generators with `go run pkg@version` or as installed binaries. Never add them via a `tool` directive in `go.mod`, which would trip depcheck.
  - depcheck computes the allowlist from `go mod graph`: modules reachable from `modernc.org/sqlite` are allowed, such as `github.com/dustin/go-humanize`, `github.com/google/uuid`, `github.com/mattn/go-isatty`, `github.com/ncruces/go-strftime` and `github.com/remyoudompheng/bigfft`.
  - depcheck also uses an explicit denylist of known BitTorrent modules as a second safeguard.

### T6. Size caps for robustness · Factory
- **Recommendation:**
  - `metadata_size` ≤ 10 MiB, matching the upload limit.
  - Piece count ≤ 2²¹, so a bitfield is at most 256 KiB.
  - A bitfield that arrives before the metadata is buffered (≤ 256 KiB) and validated once the metadata arrives.
  - Bencode nesting depth ≤ 64.
  - Incoming `request` messages for more than 16 KiB are answered with `reject_request`, or the peer is disconnected if it doesn't support the fast extension.

### T7. Schema changes before v1 · **Human**
- Several findings (G4, G10) change the §7.1 schema.
- **Recommendation:** Until the first release tag, the factory may edit `0001_init.sql` directly. After that, every change is a new migration. Any schema change goes through the architect role and is recorded as an ADR.

---

## D — Engineering defaults (Factory, listed so every agent uses the same values)

| ID | Topic | Default |
| --- | --- | --- |
| D1 | Request pipeline depth | `clamp(downloadRate × 2 s / 16 KiB, 5, 250)`; advertise `reqq=250`; honor the peer's `reqq` |
| D2 | Fast extension (BEP 6) | Send `have_all`, `have_none` and `reject_request`; send an `allowed_fast` set of k=10 to choked peers; honor received `suggest` but don't send it |
| D3 | PEX | At most 50 added and 50 dropped per message, per family; first message about 1 min after connecting |
| D4 | DHT bootstrap | `router.bittorrent.com:6881`, `dht.transmissionbt.com:6881`, `router.utorrent.com:6881`, `dht.libtorrent.org:25401` |
| D5 | DHT announcing | `get_peers` when a torrent starts and every 5 min while it needs peers; `announce_peer` every 15 min for active non-private torrents |
| D6 | LSD | `239.192.152.143:6771` and `[ff15::efc0:988f]:6771`; every 5 min per torrent; incoming announces rate-limited |
| D7 | HTTP announce | `compact=1`, `numwant=50`, a random `key` per session, `ipv6=` when there is a global v6 address |
| D8 | UDP retransmission | BEP 15 with n ≤ 8 for normal announces; the shutdown `stopped` announce makes a single attempt within the 3 s budget |
| D9 | Worker pools | Disk I/O: 4 workers; hashing: `max(1, GOMAXPROCS/2)`; file-handle LRU: 128 |
| D10 | Recent dirs | Keep the 10 most recently used; updated on each successful add |
| D11 | Peer `source` enum | `tracker`, `dht`, `pex`, `lsd`, `incoming`, `magnet` (from `x.pe`) |
| D12 | Tracker `status` enum | `not_contacted`, `updating`, `working`, `error` |
| D13 | Client identity | Peer ID `-TD0100-` + 12 random bytes; extended-handshake `v` = `torrentd 0.1.0` |
| D14 | Swarm counts | `swarmSeeders` and `swarmLeechers` are the maximum across trackers that responded recently |

---

## Next steps

1. **Human:** Review the 10 Human items (C2, C3, C4, G8, G9, G10, G15, A13, A14, T7). Skim the Factory items and override any you disagree with.
2. **Factory:** Write the accepted decisions into `spec/decisions/` as ADRs and add a "Spec amendments" section to `project-description.md`.
3. **Factory:** Seed `spec/acceptance.yaml` with checks for every testable decision (for example C1 becomes an SSE payload-size test, G17 a Host-header rejection test, and C3 a kill-9 resume test).
4. **Factory retro note:** This audit was done by a single agent in one pass. To evaluate the *auditor role* itself, run 2–3 independent auditor agents on the same spec and compare what they find. Findings that only one auditor caught show how much variance there is between auditors.
