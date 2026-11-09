You are the **implementer** of milestone {{MS}} of `torrentd` (Approach A+R). Your implementation passes the acceptance gate. Independent reviewers and a red team have now examined it. Their findings are below. You have **one fix round**.

## Findings

{{FINDINGS}}

## Rules for the fix round

1. **Reproduce first.**
   - **Red-team findings** already come with a failing test in `harness/acceptance/redteam/m{{MS}}/`. Run it.
   - **For a reviewer finding,** write a unit test (in your own `_test.go` files) or a small scenario that demonstrates the problem **before** you fix it.
   - If you can't reproduce a finding after a reasonable attempt, **don't change code for it.** List it as "not reproduced", with what you tried.
2. **Fix what you reproduced,** properly and at the root cause. Don't add test-specific special cases.
3. **Keep everything green:**
   - `harness/factory gate --upto {{MS}}` must still exit 0;
   - the red-team tests you addressed must pass: `cd harness && go test ./acceptance/redteam/m{{MS}}/ -run <Test>`;
   - `go vet` and `-race` must be clean;
   - the coverage floors must hold.
4. **Constraints:**
   - You can't edit `contract.go` / `contract_test.go`, acceptance tests, red-team tests or the harness. If a finding needs a contract change, write `escalations/M{{MS}}-fix-<slug>.md`.
   - Don't use subagents. Write files with the Write/Edit tools. Commit with the attribution your environment specifies. Never push, and never switch branches.

## Final report

For each finding, report one of **fixed**, **not reproduced** or **escalated**, with a one-line reason. Paste the final `harness/factory gate --upto {{MS}}` progress table verbatim.
