# StartPaused: Add must return `paused`, but the contract also says it returns `checking`
- Role: implementer   Ticket/IDs: ADR-0016/0019, session contract
- Blocking: no

## What I found
The session contract contradicts its own frozen test about the state that `Add` returns when `StartPaused` is set.

## What the spec/harness says
- `internal/bt/session/contract.go`, Session.Add: "On success it returns the initial Status (state checking)". AddOptions.StartPaused: "it is still checked (local only), then goes to paused, or to stopped with StopReason user if the check found it complete."
- `internal/bt/session/contract_test.go` TestContractSessionAddRules: `if st.State != StatePaused { t.Fatalf("StartPaused state = %s, want paused") }`. This is checked on the Status that Add returns, and again 300 ms later.

## Options I see
1. Report `paused` from the start, and still run the local check behind it (what I did).
2. Skip the check for StartPaused adds. That loses the "complete data → stopped/user" rule.
3. The architect changes either the doc or the test.

## What I did meanwhile
For a StartPaused add, the actor runs the local check (no network). While that check runs, the published Status shows `paused` instead of `checking` (`torrent.hideCheck`). When the check finishes, the torrent goes to `paused`, or to `stopped`/`user` if the data is complete. Restored torrents and Resume still show `checking` normally.
