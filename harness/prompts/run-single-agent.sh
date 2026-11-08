#!/usr/bin/env bash
# Launch an Approach A run: one headless implementer session on one milestone,
# in its own git worktree and branch, with a full transcript for the retro.
#
#   harness/prompts/run-single-agent.sh [--base REF] [--tag NAME] <milestone> [budget-usd]
#
# --base starts the branch from REF instead of main (e.g. to rerun a milestone
# from the same starting point as another approach); --tag changes the
# branch/worktree/run suffix (default: approach-a).
#
# Creates ../<repo>-m<N>a on branch impl/m<N>-approach-a from main, renders
# the prompt (single-agent-implementer.md + scope-m<N>.md) and writes
# runs/approach-a-m<N>/{prompt.md,transcript.jsonl,stderr.log,started_at,finished_at,exit_code}
# in the main checkout. Runs in the foreground; background it if you like.
set -euo pipefail
# Keep macOS from idle/system sleeping during a run: a sleeping machine kills
# in-flight responses ("Your computer went to sleep mid-response"). Lid-close
# sleep can't be prevented this way; keep the lid open (or use clamshell on power).
AWAKE=(); command -v caffeinate >/dev/null && AWAKE=(caffeinate -ims)
BASEREF=main; TAG=approach-a; ROLE=implementer
while [ "${1:-}" = "--base" ] || [ "${1:-}" = "--tag" ] || [ "${1:-}" = "--role" ]; do
  case "$1" in --base) BASEREF="$2";; --tag) TAG="$2";; --role) ROLE="$2";; esac; shift 2
done
MS="${1:?usage: run-single-agent.sh [--base REF] [--tag NAME] <milestone> [budget-usd]}"
BUDGET="${2:-25}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PROMPTS="$ROOT/harness/prompts"
SCOPE="$PROMPTS/scope-m$MS.md"
BRANCH="impl/m$MS-$TAG"
WT="$(dirname "$ROOT")/$(basename "$ROOT")-m${MS}-$TAG"
RUN="$ROOT/runs/$TAG-m$MS"

[ -f "$SCOPE" ] || { echo "missing $SCOPE"; exit 2; }
if [ -n "$(find "$ROOT/harness/_staged/m$MS" -type f ! -name '.*' 2>/dev/null)" ]; then
  echo "milestone $MS acceptance tests are still staged; as verifier/human run:" >&2
  echo "  harness/factory promote --ms $MS && git commit -am 'Promote M$MS acceptance tests'" >&2
  exit 2
fi
[ -z "$(git -C "$ROOT" status --porcelain)" ] || { echo "main checkout is dirty; commit first" >&2; exit 2; }
[ ! -e "$WT" ] || { echo "$WT already exists" >&2; exit 2; }

git -C "$ROOT" worktree add -q "$WT" -b "$BRANCH" "$BASEREF"
[ -f "$WT/e2e/package-lock.json" ] && (cd "$WT/e2e" && npm ci --no-audit --no-fund >/dev/null)

mkdir -p "$RUN"
python3 - "$PROMPTS/single-agent-implementer.md" "$SCOPE" "$MS" "$BRANCH" "$ROLE" > "$RUN/prompt.md" <<'EOF'
import sys
tpl, scope, ms, branch, role = sys.argv[1:6]
s = open(tpl).read().replace("{{SCOPE}}", open(scope).read().strip())
s = s.replace("{{MS}}", ms).replace("{{BRANCH}}", branch)
if role == "architect":
    s = s.replace("You are the **implementer**", "You are the **single agent** (architect and implementer in one)")
    s = s.replace("--role implementer", "--role architect")
    s += ("\n\n## Your role\n\nYou hold the **architect** role as well. You may evolve `contract.go`/`contract_test.go` "
          "files and add ADRs in `spec/decisions/` (record every contract change in an ADR). "
          "You still may not edit `spec/acceptance.yaml`, `harness/`, `e2e/` or any `*_acceptance_test.go`.\n")
print(s, end="")
EOF

date -u +%Y-%m-%dT%H:%M:%SZ > "$RUN/started_at"
set +e
(cd "$WT" && FACTORY_ROLE="$ROLE" "${AWAKE[@]}" claude -p "$(cat "$RUN/prompt.md")" \
  --permission-mode auto --output-format stream-json --verbose \
  --max-budget-usd "$BUDGET" -n "approach-a-m$MS" \
  > "$RUN/transcript.jsonl" 2> "$RUN/stderr.log")
echo "exit=$?" > "$RUN/exit_code"
date -u +%Y-%m-%dT%H:%M:%SZ > "$RUN/finished_at"
tail -1 "$RUN/transcript.jsonl" | jq -r '"\(.subtype) turns=\(.num_turns) cost=$\(.total_cost_usd) duration=\((.duration_ms/60000)|floor)min"' 2>/dev/null || true
echo "worktree: $WT   branch: $BRANCH   run: $RUN"
