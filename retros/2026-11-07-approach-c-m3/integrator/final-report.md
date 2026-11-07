Milestone 3 is delivered and `harness/factory gate --upto 3` exits 0 with 54/54 passing, but one part of the definition of done is not met. `harness/factory integrity --base main --role implementer` **fails**. The cause is a process gap, not the code (explained below). `make build` works, `go vet ./...` is clean, and everything is committed on `impl/m3-c`. Nothing was pushed.

## Final `harness/factory gate --upto 3` table (verbatim)
```
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
ADD-01     3   L3   pass     
TRK-01     3   L3   pass     
TRK-02     3   L3   pass     
TRK-03     3   L3   pass     
WIRE-01    3   L3   pass     
WIRE-02    3   L3   pass     
WIRE-04    3   L3   pass     
WIRE-05    3   L3   pass     
STOR-01    3   L3   pass     
STOR-02    3   L3   pass     
STOR-04    3   L3   pass     
STOR-05    3   L3   pass     
STOR-06    3   L3   pass     
STOR-07    3   L3   pass     
SP-01      3   L4   pass     
QUAL-01    1   L0   pass     
QUAL-02    1   L0   pass     

Progress
  M1    18/18  pass (100.0%)   fail 0   missing 0   error 0   manual 0
  M2    21/21  pass (100.0%)   fail 0   missing 0   error 0   manual 0
  M3    15/15  pass (100.0%)   fail 0   missing 0   error 0   manual 0
  ALL   54/54  pass (100.0%)   fail 0   missing 0   error 0   manual 0
```
All 15 M3 checks passed on the first gate run after wiring, and again on a second run. SP-01 ran against the real libtorrent seeder in Docker. The other checks also pass:
- **Coverage:** session 95.0%, api 92.8%, and the four work packages between 97.6% and 100%.
- **Quality, dependencies, clock lint:** `quality-check.sh`, `depcheck.sh` and the clock lint all pass.
- **Unit tests:** `go test -race ./...` is green.

## Merge results
The four branches merged with `--no-ff` in the order tracker, peer, piece, storage. There were **no conflicts**, and `go build ./...` passed after each merge. Every package's contract tests and own tests passed with `-race` straight after merging. I didn't change any work-package code.

## What broke at integration, and why
Almost nothing. I found no contract gaps, misread contracts or work-package bugs.
- **Session contract test:** passed on the first run, and 10 repeated runs were also clean.
- **API:** I generalised the preview upload reader so it keeps the other form fields (`downloadDir`, `startPaused`, `magnet`). That broke an existing M2 test, `TestPreviewErrors`: a huge non-file field now gave 400 instead of 413. I fixed it by reading the rest of an oversized field, so the body-size limit still produces the 413.
- **My own unit tests:** two mistakes in the tests themselves: a fixture with no tracker client, and a wrong expected request count. Neither was a product bug.
- **Integration hazards I designed around** (the contracts didn't cover these):
  - **Message ordering:** the session calls `peer.NewConn` inside the actor, so a peer is registered before any of its messages can arrive.
  - **Hash timing:** a complete piece is hashed only after every write for that piece has finished, because writes of one piece can complete out of order.

These are the most useful data for the retro. The plan's detailed integration notes (limits, pitfalls, the route-table workaround) left little to discover.

## Escalations
- **New: `escalations/M3-integration-integrity-base.md`.** The integrity check lists 18 protected files (contracts, ADRs 0012–0015, `m3.json`). They all come from the architect's 4 commits (`9419c01`–`8ed1518`), which sit on `impl/m3-c` but were never merged into `main`. Nothing after those commits touches a protected file: the diff is empty, and `integrity --base 8ed1518 --role implementer` passes. I didn't rewrite history to make the check pass.
- **Already open, from the architect:** `M3-piece-order-vs-random-first.md` (M4 needs to resolve it) and `M3-shutdown-order.md`. The second is implemented as ADR-0015 says: HTTP shutdown and `Session.Close` start together.

## What I'd change about the contracts or the plan
1. **Put the contract commits on `main`** (or a tagged base) before the fan-out, so every role's integrity check has a correct base. Alternatively, the harness could take a role per commit range.
2. **Add a session test helper to the frozen contract,** such as a fake `peer.Conn`. I wrote one in my own test file, but every integrator will need it.
3. **Pin two things the session contract leaves open:**
   - **Write-then-verify ordering:** a piece may only be hashed after all of its block writes have finished.
   - **Who calls `NewConn`:** the actor, to avoid a race with incoming messages.

   Both were easy to get subtly wrong, and only my design kept the black-box gate from catching them late.
4. **A small spec addition:** the default peer port when `--peer-port` is absent. I used the `listen_port` setting, otherwise 42069 from §7. This and my other interpretations are listed in `PROGRESS.md`.
