#!/usr/bin/env bash
# Launch an Approach A run: one headless implementer session on one milestone,
# in its own git worktree and branch, with a full transcript for the retro.
#
#   harness/prompts/run-single-agent.sh <milestone> [budget-usd]
#
# Creates ../<repo>-m<N>a on branch impl/m<N>-approach-a from main, renders
# the prompt (single-agent-implementer.md + scope-m<N>.md) and writes
# runs/approach-a-m<N>/{prompt.md,transcript.jsonl,stderr.log,started_at,finished_at,exit_code}
# in the main checkout. Runs in the foreground; background it if you like.
set -euo pipefail
MS="${1:?usage: run-single-agent.sh <milestone> [budget-usd]}"
BUDGET="${2:-25}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PROMPTS="$ROOT/harness/prompts"
SCOPE="$PROMPTS/scope-m$MS.md"
BRANCH="impl/m$MS-approach-a"
WT="$(dirname "$ROOT")/$(basename "$ROOT")-m${MS}a"
RUN="$ROOT/runs/approach-a-m$MS"

[ -f "$SCOPE" ] || { echo "missing $SCOPE"; exit 2; }
if [ -d "$ROOT/harness/_staged/m$MS" ]; then
  echo "milestone $MS acceptance tests are still staged; as verifier/human run:" >&2
  echo "  harness/factory promote --ms $MS && git commit -am 'Promote M$MS acceptance tests'" >&2
  exit 2
fi
[ -z "$(git -C "$ROOT" status --porcelain)" ] || { echo "main checkout is dirty; commit first" >&2; exit 2; }
[ ! -e "$WT" ] || { echo "$WT already exists" >&2; exit 2; }

git -C "$ROOT" worktree add -q "$WT" -b "$BRANCH" main
[ -f "$WT/e2e/package-lock.json" ] && (cd "$WT/e2e" && npm ci --no-audit --no-fund >/dev/null)

mkdir -p "$RUN"
python3 - "$PROMPTS/single-agent-implementer.md" "$SCOPE" "$MS" "$BRANCH" > "$RUN/prompt.md" <<'EOF'
import sys
tpl, scope, ms, branch = sys.argv[1:5]
s = open(tpl).read().replace("{{SCOPE}}", open(scope).read().strip())
print(s.replace("{{MS}}", ms).replace("{{BRANCH}}", branch), end="")
EOF

date -u +%Y-%m-%dT%H:%M:%SZ > "$RUN/started_at"
set +e
(cd "$WT" && FACTORY_ROLE=implementer claude -p "$(cat "$RUN/prompt.md")" \
  --permission-mode auto --output-format stream-json --verbose \
  --max-budget-usd "$BUDGET" -n "approach-a-m$MS" \
  > "$RUN/transcript.jsonl" 2> "$RUN/stderr.log")
echo "exit=$?" > "$RUN/exit_code"
date -u +%Y-%m-%dT%H:%M:%SZ > "$RUN/finished_at"
tail -1 "$RUN/transcript.jsonl" | jq -r '"\(.subtype) turns=\(.num_turns) cost=$\(.total_cost_usd) duration=\((.duration_ms/60000)|floor)min"' 2>/dev/null || true
echo "worktree: $WT   branch: $BRANCH   run: $RUN"
