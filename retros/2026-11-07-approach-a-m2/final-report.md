# M2 report: Approach A (single implementer)

Milestone 2 is done. `harness/factory gate --upto 2` exits 0 with 39 of 39 checks passing. Integrity passes, `make build` produces `./torrentd`, `go vet ./...` is clean and `git status` is clean. All 21 M2 checks passed the first time I ran the gate on the implementation.

## Final `harness/factory gate --upto 2` (verbatim)
```
ID         MS  TIER STATUS   DETAIL
AC-01      1   L6   pass     
AC-02      1   L0   pass     
AC-13      1   L0   pass     
SKEL-01    1   L3   pass     
SKEL-02    1   L3   pass     
SKEL-03    1   L3   pass     
SKEL-04    1   L3   pass     
SKEL-05    1   L3   pass     
SKEL-06    1   L3   pass     
SKEL-07    1   L3   pass     
SEC-01     1   L3   pass     
SEC-02     1   L3   pass     
SEC-03     1   L3   pass     
DEP-01     1   L3   pass     
DEP-02     1   L3   pass     
LINT-01    1   L0   pass     
BENC-01    2   L1   pass     
BENC-02    2   L1   pass     
BENC-03    2   L1   pass     
BENC-04    2   L1   pass     
BENC-05    2   L1   pass     
BENC-06    2   L2   pass     
MAG-01     2   L1   pass     
MAG-02     2   L1   pass     
META-01    2   L3   pass     
META-02    2   L3   pass     
META-03    2   L3   pass     
META-04    2   L3   pass     
META-05    2   L3   pass     
META-06    2   L3   pass     
META-07    2   L3   pass     
META-08    2   L3   pass     
META-09    2   L3   pass     
META-10    2   L3   pass     
META-11    2   L3   pass     
PREV-01    2   L3   pass     
PREV-02    2   L3   pass     
QUAL-01    1   L0   pass     
QUAL-02    1   L0   pass     

Progress
  M1    18/18  pass (100.0%)   fail 0   missing 0   error 0   manual 0
  M2    21/21  pass (100.0%)   fail 0   missing 0   error 0   manual 0
  ALL   39/39  pass (100.0%)   fail 0   missing 0   error 0   manual 0
```
- **Coverage:** bencode 97.5%, metainfo 100%, api 99.5%.
- **Fuzzing:** besides the gate's 30 s run, I ran `FuzzDecode` for another 90 s (about 40 million executions) with no failures.

## Commits (on `impl/m2-approach-a`, not pushed)
- `92741e7` M2: bencode codec, metainfo parsing, magnet links, /torrents/preview
- `1d0fde9` M2: unit tests for bencode, metainfo and preview; PROGRESS.md

## Escalations
None. Nothing in the spec contradicted the fixtures, and I didn't need any contract changes.

## Interpretations I chose (full list in PROGRESS.md)
- **Lenient mode:** it accepts what strict mode rejects only on canonical form: unsorted or duplicate keys (the last one wins), leading zeros and `-0`. It still rejects `ie`, `i-e`, non-digits, truncated input, non-string keys and trailing data. This is the literal reading of "strict additionally rejects".
- **Integer overflow:** a value beyond int64, or beyond the target type, is an error.
- **Extra types:** `bool` is encoded as `i0e`/`i1e`. `[N]byte` is encoded as a string and decoded only from a string of exactly N bytes.
- **Empty values:** nil pointers, nil interfaces and empty `RawMessage`s are left out of structs. At top level or inside a list they are an error.
- **Trackers:**
  - `announce` and `announce-list` are decoded tolerantly: a mistyped one is ignored rather than rejecting the torrent.
  - Blank and duplicate URLs and empty tiers are dropped, and tier numbers stay contiguous.
  - `announce` becomes tier 0 only when the announce-list yields nothing.
  - URLs are not validated, because the oracle keeps the scheme-less URL in `sample.torrent`.
- **Sizes:** a single zero-length file is allowed. A torrent whose non-pad total is 0 is rejected, and so is a total that overflows.
- **Pad files:**
  - They need a valid path.
  - They count toward piece count and offsets.
  - They are left out of duplicate detection, because hybrids repeat `.pad/N`, and out of `files[]` and `sizeBytes`.
- **v2-only:** detected as "no `pieces` and `meta version` ≥ 2".
- **Name:** validated and sanitized like a path component.
- **Sanitizing:**
  - `<>:"|?*`, control characters and invalid UTF-8 become `_`.
  - Each trailing dot or space becomes `_`.
  - A reserved stem gets `_` appended: `CON` → `CON_`, `nul.txt` → `nul_.txt`.
- **Duplicate paths:** checked on the sanitized path after NFC and case folding, using the new dependency `golang.org/x/text`, which §2 allows.
- **Upload limit:**
  - The `file` part may be up to 10 MiB. The whole body may be 1 MiB larger to allow for multipart overhead. Going over either limit returns 413.
  - A non-multipart body, a missing `file` part or a malformed body returns 400 `invalid_request`.
  - GET on the endpoint returns 405 with `Allow: POST`.
- **Other limits:** more than 2²¹ pieces is rejected (T6). A non-power-of-two piece length is accepted and logged at INFO.

## Where the spec or harness slowed me down
Almost nowhere: this run took about 2 commits and $2.30.
- **What made it easy:** the oracle fixtures, each with a `rule` and a `why`, together with the precise tests left little to guess.
- **Upload limit wording:** the 10 MiB limit applies to the file part, not the request body. The test makes this clear, but OpenAPI only says "max 10 MiB" on the field. One sentence in the spec would settle it.
- **Pad-file details:** pad files' paths and their place in duplicate detection aren't in §16.1. Hybrid torrents force the choice in practice (they repeat `.pad/N` paths), but the spec could state it.
- **Lenient mode:** whether it accepts `i03e` or `-0` is only implied by the word "additionally". Fuzz stability holds either way.

## What I'd change about these instructions
- **Time limits:** the "3 serious attempts" rule had nothing to act on this time. It would help to also say how long to spend on hardening beyond the gate, such as extra fuzz time.
- **Coverage:** the definition of done in the brief lists only gate, integrity, build and git. Coverage and quality are inside the gate through QUAL-01 and QUAL-02, so saying that explicitly would avoid running those checks twice.
