# M1 report: Approach A (single implementer)

Milestone 1 is not fully done: the gate shows 17 of 18 entries passing, and `gate --ms 1` exits 1. The one failure is QUAL-01, and it can't pass at M1 without starting milestone 2, which I was told not to do. All other done criteria hold: integrity passes, `make build` produces a working `./torrentd`, `go vet ./...` is clean, and `git status` is clean.

## Final `harness/factory gate --ms 1` (verbatim)
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
QUAL-01    1   L0   fail     exit code 1
QUAL-02    1   L0   pass     

Progress
  M1    17/18  pass ( 94.4%)   fail 1   missing 0   error 0   manual 0
  ALL   17/18  pass ( 94.4%)   fail 1   missing 0   error 0   manual 0
```
In the QUAL-01 detail, only two packages fail: `internal/bt/bencode: tests fail` and `internal/bt/metainfo: tests fail`. Every M1 package is above its floor: `.` 75%, api 99%, clock 100%, config 100%, store 89%, depcheck 96%.

## Commits (on `impl/m1-approach-a`, not pushed)
- `32c3fd5` M1: product module, config, store with migrations, clock, HTTP skeleton
- `be4d010` M1: tools/depcheck, api and depcheck unit tests, Makefile
- `c7510ff` M1: PROGRESS.md and escalations

## Escalations
1. **`escalations/M1-qual-gates-include-m2-packages.md` (blocking)**
   - **Problem:** `main` already contains the verifier's M2 test files in `internal/bt/{bencode,metainfo}`. Without M2 code they don't compile, so `go vet ./...` and staticcheck fail (QUAL-02). Coverage then records "tests fail" for those packages (QUAL-01).
   - **Why I couldn't fix it:** QUAL-01 says "enforced from the milestone that introduces each package", but `factory coverage` doesn't filter by milestone. The harness was only ever validated with M1 and M2 built together.
   - **What I did:** added stubs with exactly the pinned M2 signatures, each returning "not implemented (milestone 2)". That makes the tree compile, so QUAL-02 passes and `go vet` is clean. I didn't implement M2.
   - **Why I stopped:** the only remaining ways to turn QUAL-01 green are doing M2 or changing the harness, and both are off-limits. Making three "attempts" would have meant nothing.
2. **`escalations/M1-bash-hook-false-positives.md` (not blocking)**
   - **Problem:** the Bash hook blocked three harmless commands. Each one merely *mentioned* `spec/contracts/go-api.md` or `harness/factory`, for example in a doc comment, in the Makefile's `gate` target, or by running `harness/factory` in the same call as a file write.
   - **What I did:** switched to the Write/Edit tools and ran `harness/factory` in separate calls. These denials show up in `runs/policy-denials.jsonl`, but none of them is a gaming attempt.

## Interpretations I chose
These are also recorded in PROGRESS.md.
- **Wrong method on a non-API path** (e.g. POST `/healthz`): returns 405 with code `invalid_request`. The error-code list has no `method_not_allowed`.
- **`--dev` mode:** GET `/` returns a 404 JSON instead of the app.
- **Host header without a port:** treated as port 80.
- **Same-origin:** `http://` plus an allowed `host:port`. `Origin: null` is rejected.
- **Clean-shutdown marker:** implemented now, because it's cheap and fits the shutdown steps that already exist.
- **Default settings:** not seeded yet. That belongs with `/settings` in milestone 6.

## Where the spec or harness slowed me down
- **Biggest issue:** the M1 gate can't go green on M1 work alone, because acceptance tests for milestone N+1 sit in product packages and every check over `./...` picks them up. The fix should be either milestone-scoped QUAL checks or not committing next-milestone tests early.
- **The Bash hook heuristic:** it cost about three retries.
- **Otherwise smooth:** the M1 tests were precise and matched the contract. Every SKEL, SEC and DEP check passed on the first run.

## What I'd change about the instructions
- Say explicitly what to do when the definition of done conflicts with the scope limit. Here, "gate --ms 1 exits 0" can't hold together with "don't start M2".
- Either allow compiling stubs of pinned APIs for future milestones, or remove those tests from the tree before M1 starts.
- Tell implementers to use Write/Edit for file content and to run `harness/factory` in its own Bash call.
- Settle the attribution line: the brief asks for `Co-Authored-By: Claude <noreply@anthropic.com>`, while the harness asks for "Claude Opus 5.5". I followed the brief.
