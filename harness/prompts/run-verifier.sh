#!/usr/bin/env bash
# Approach V: a verifier agent builds a milestone's acceptance harness.
#   harness/prompts/run-verifier.sh <milestone> [budget-usd]
# Branch verify/m<N>, worktree ../<repo>-m<N>-verify, run dir runs/verifier-m<N>/.
set -euo pipefail
AWAKE=(); command -v caffeinate >/dev/null && AWAKE=(caffeinate -ims)
MS="${1:?usage: run-verifier.sh <milestone> [budget-usd]}"; BUDGET="${2:-35}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"; P="$ROOT/harness/prompts"
SCOPE="$P/scope-m$MS.md"; BRANCH="verify/m$MS"; WT="$(dirname "$ROOT")/$(basename "$ROOT")-m$MS-verify"; RUN="$ROOT/runs/verifier-m$MS"
[ -f "$SCOPE" ] || { echo "missing $SCOPE" >&2; exit 2; }
[ -z "$(git -C "$ROOT" status --porcelain)" ] || { echo "main checkout is dirty; commit first" >&2; exit 2; }
[ ! -e "$WT" ] || { echo "$WT exists" >&2; exit 2; }
git -C "$ROOT" worktree add -q "$WT" -b "$BRANCH" main
(cd "$WT/e2e" && npm ci --no-audit --no-fund >/dev/null)
mkdir -p "$RUN"
python3 - "$P/verifier-harness.md" "$SCOPE" "$MS" "$BRANCH" > "$RUN/prompt.md" <<'PY'
import sys
tpl, scope, ms, br = sys.argv[1:5]
s = open(tpl).read().replace("{{SCOPE}}", open(scope).read().strip())
print(s.replace("{{MS}}", ms).replace("{{BRANCH}}", br), end="")
PY
date -u +%Y-%m-%dT%H:%M:%SZ > "$RUN/started_at"
set +e
(cd "$WT" && FACTORY_ROLE=verifier "${AWAKE[@]}" claude -p "$(cat "$RUN/prompt.md")" --permission-mode auto \
  --output-format stream-json --verbose --max-budget-usd "$BUDGET" -n "verifier-m$MS" > "$RUN/transcript.jsonl" 2> "$RUN/stderr.log")
echo "exit=$?" > "$RUN/exit_code"; set -e
date -u +%Y-%m-%dT%H:%M:%SZ > "$RUN/finished_at"
tail -1 "$RUN/transcript.jsonl" | jq -r '"\(.subtype) turns=\(.num_turns) cost=$\(.total_cost_usd) \((.duration_ms/60000)|floor)min | \(.result|.[0:80])"' 2>/dev/null || true
echo "worktree: $WT   branch: $BRANCH   run: $RUN"
