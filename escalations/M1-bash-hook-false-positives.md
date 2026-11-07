# The Bash write-policy hook blocks harmless commands that only mention protected paths
- Role: implementer   Ticket/IDs: M1 (process, no acceptance ID)
- Blocking: no

## What I found
`harness/hooks/pre-tool-use.sh` blocked three Bash calls during M1, and none
of them wrote a protected file:
1. A heredoc writing `internal/config/config.go`, whose Go doc comment
   *mentioned* `spec/contracts/go-api.md` → "may not run shell commands that
   write to spec/contracts/go-api.md".
2. A command that edited `tools/depcheck/main.go` with an inline script and
   then *ran* `harness/factory gate ...` → "write to harness/factory".
3. Writing `Makefile` with `printf > Makefile` when the content contained the
   text `harness/factory gate --ms 1` (the required `make gate` target), and
   separately `git commit -m "..." && harness/factory integrity ...`.

The heuristic seems to flag any command that both looks like a write and
mentions a protected path anywhere. Running `harness/factory` is something
every role is told to do (CLAUDE.md "Commands").

## What the spec/harness says
- `harness/README.md` "Known limitations": "The Bash part of the hook is a
  heuristic. The integrity check is the real enforcement."
- CLAUDE.md: "If a write is blocked, don't work around it. Escalate."

## Options I see
1. Treat the *target* of redirections/`tee`/`sed -i`/`cp`/`mv` as the write
   set, not every path mentioned in the command (heredoc bodies included).
2. Allowlist executing `harness/factory` (and `make gate`) as a non-write.
3. Leave it as is, and tell implementers in CLAUDE.md to use the Write/Edit
   tools for file content and keep `harness/factory` runs in their own Bash
   call.

## What I did meanwhile
I didn't work around the policy for any protected file. I used the
Write/Edit tools, which check the real target path, for product files, and
ran `harness/factory` in separate Bash calls. `factory integrity` passes. Each
denial is in `runs/policy-denials.jsonl`. They are false positives, not
gaming attempts.
