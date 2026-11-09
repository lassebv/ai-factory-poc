# M4 final report (Approach A+, implementer)

Milestone 4 is not fully done: `harness/factory gate --upto 4` passes 79 of 80 checks and exits 1. The one gap is AC-06. Its check points at a test package, `./tools/swarm`, that doesn't exist, and as implementer I can't write a test that would count. All other M4 checks pass, including SEED-02 against libtorrent in Docker and RACE-01. M1–M3 still pass.

The rest of the definition of done holds:
- `harness/factory integrity --base 93cf5b5… --role implementer` passes (35 changed files, all writable by the implementer).
- `make build` produces `./torrentd` and `go vet ./...` is clean.
- The coverage floors, quality checks and dependency check pass.
- `git status` is clean.

## Final `harness/factory gate --upto 4` progress table
```
Progress
  M1    18/18  pass (100.0%)   fail 0   missing 0   error 0   manual 0
  M2    21/21  pass (100.0%)   fail 0   missing 0   error 0   manual 0
  M3    15/15  pass (100.0%)   fail 0   missing 0   error 0   manual 0
  M4    25/26  pass ( 96.2%)   fail 0   missing 1   error 0   manual 0
  ALL   79/80  pass ( 98.8%)   fail 0   missing 1   error 0   manual 0
```
(AC-06: `missing — package directory ./tools/swarm does not exist`)

## Commits (on `impl/m4-aplus`, oldest first)
1. `4a8941b` piece: M4 picker (ownership, rarest-first buckets, endgame, expiry, exclusive retry)
2. `902eecf` choke: tit-for-tat choker with optimistic rotation, anti-snub, Fill; sliding-window meter
3. `f820112` storage: process-wide pools, async verify, Sync, Stat fingerprint, PlanResume
4. `927b4ea` peer: AcceptHandshake and OnPieceSent
5. `64b065c` store: torrents and bans persistence
6. `9f5478a` session: M4 engine (seeding, choking, endgame/expiry, bans, pools data path, persistence, fast resume, lifecycle)
7. `1faa3e1` api, cmd: lifecycle routes, stopReason/checkingProgress JSON, store-backed engine with fast resume
8. `c0b7ccd` session: stale-request pipeline accounting, periodic top-up, no redial backoff after our own teardown; unit tests; escalations
9. `cf2fe14` btest: use the injected real clock (LINT-01); PROGRESS for M4
10. `1127af1` PROGRESS: M4 status

## Escalations and interpretations
- **`escalations/M4-ac06-missing-test-package.md`:** AC-06 is unbuildable for the implementer. Either the verifier provides `tools/swarm/*_acceptance_test.go`, or AC-06 is pointed at STOP-01, which tests the same behaviour.
- **`escalations/M4-contract-startpaused-initial-state.md`:** the session contract contradicts its own frozen test.
  - The doc says Add returns `checking`, and a StartPaused torrent is still checked.
  - `TestContractSessionAddRules` requires Add to return `paused`.
  - What I did: the local check still runs, but it is reported as `paused`. When it finishes, the torrent goes to `paused`, or to `stopped`/`user` if the data is complete.
- **Stale requests (RES-04):** a peer that never answers some pieces clogged the pipeline until no request could be sent. The 20 s expiry plus 20 s cooldown can't recover within the test's 30 s window.
  - Fix, in the session rather than the frozen contract: a request outstanding longer than max(1 s, 4 × the peer's smoothed latency) no longer counts against that peer's pipeline depth.
  - Outstanding requests per peer stay capped at 250, and every 1 s tick tops up all pipelines.
- **Redial after pause (PAUSE-01):** peers we close ourselves on pause or stop don't get the redial backoff, so resume reconnects at once.
- **Resume from error:** runs a full recheck.
- **A restored torrent with desired state `paused` whose data is complete:** goes to `stopped`/`user`.
- **LINT-01 was already failing at the base commit.** The architect's `internal/bt/btest/btest.go` used `time.Now`/`time.Sleep` directly. That file is product-owned, so I switched it to `clock.Real()`.

## Where the spec or harness slowed me down
- **The acceptance tests print no torrentd logs on failure.** RES-04's only output was "progress 0.00". I found the cause by simulating the picker with a throwaway test, not from evidence.
- **RES-04 constrains behaviour the spec doesn't state.** It needs some stall detection well below the 20 s request timeout fixed in ADR-0018. Neither the spec nor that ADR says how a pipeline should behave when a peer silently withholds some pieces.
- **The architect's handoff had three defects:**
  - the contract doc and its own frozen test disagree about StartPaused;
  - AC-06 points at a package nobody can create;
  - the architect-supplied helpers broke LINT-01.

  A gate run on the architect's handoff commit (expecting only "not implemented yet" failures) would have caught all three before implementation started.

## What I would change about these instructions
- Require the architect to run `gate --upto <N-1>` and a dry run of the milestone's gate on the contract commit, so that `missing` entries and lint regressions are fixed before the implementer starts.
- Make the acceptance harness print the torrentd log tail on every failing check, not just some of them.
- Say explicitly whether the implementer may fix lint or quality problems in architect-supplied files that are product-owned, such as `btest`. I assumed yes, because the policy allows it and the risk review says changes to `btest` should be reviewed in the integrity diff.
