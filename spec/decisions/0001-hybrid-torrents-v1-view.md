# ADR-0001: Accept hybrid torrents through their v1 view

- **Status:** Accepted (2026-11-06)
- **Audit finding:** C2 · **Spec:** §16.1

## Context
BEP 52 and hybrid torrents are a non-goal (§1.2). Interop with qBittorrent in both directions is required (§11, §14.10). Recent libtorrent-2.x-based qBittorrent builds can create hybrid v1+v2 torrents, and many published torrents are hybrid. Hybrid torrents contain BEP 47 padding files.

## Decision
- Accept hybrid torrents and treat them as v1 torrents: v1 info-hash, v1 `pieces`. Ignore `meta version`, `file tree` and `piece layers`.
- Support BEP 47 padding files: never write them to disk, exclude them from every size and file list shown to the user, and treat them as zeros when hashing.
- Reject v2-only torrents with `invalid_torrent`.

## Consequences
- The client interoperates with real-world torrents. The cost is modest: pad-file handling in metainfo, storage and the piece picker.
- Full v2 (merkle verification, v2 swarms) stays out of scope.
- The interop harness tests both v1 and hybrid fixtures.
