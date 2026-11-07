**Milestone 2 (Bencode + metainfo)**, §15.2. Milestone 1 is already merged.

- `internal/bt/bencode`: the encoder and decoder. Implement the pinned API in `spec/contracts/go-api.md` exactly, with strict and lenient modes (§16.1 A1), `RawMessage`, struct tags and the size/depth caps (§16.2 T6). It must survive fuzzing. Replace the milestone 1 placeholder stubs.
- `internal/bt/metainfo`: `.torrent` parsing and validation (§4.1 Metainfo, §16.1): single-file and multi-file torrents, hybrid torrents through their v1 view, BEP 47 pad files, path safety and sanitization, piece validation, UTF-8 fields, and an info-hash computed from the raw bytes. Also implement the pinned `ParseMagnet`. Replace the milestone 1 placeholder stubs.
- `POST /api/v1/torrents/preview` in `internal/api`, exactly as specified in `spec/openapi.yaml`, including the 10 MiB upload limit and the error codes.
- Unit tests for your own code, enough to meet the coverage floors (≥ 80% for `internal/bt/*`).

**Out of scope:** adding torrents (`POST /api/v1/torrents`), storage, trackers, the peer wire protocol, ut_metadata, and the frontend.

The black-box fixtures in `harness/fixtures/torrents/expected.json` come from independent oracles (libtorrent and Transmission). If your parser disagrees with them, your parser is the thing that's wrong. The one exception: if you believe a verdict contradicts the spec, escalate it.
