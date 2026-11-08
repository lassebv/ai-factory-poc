#!/usr/bin/env bash
# Launch an Approach A+ run: an architect (contracts, contract tests, ADRs,
# risk review), then ONE implementer that builds everything against them.
#
#   harness/prompts/run-aplus.sh [--base REF] [--from-phase 2] <milestone> [arch-budget] [impl-budget]
#
# Branch impl/m<N>-aplus, worktree ../<repo>-m<N>-aplus, run dir runs/aplus-m<N>/.
set -euo pipefail
AWAKE=(); command -v caffeinate >/dev/null && AWAKE=(caffeinate -ims)
BASEREF=main; FROM=1
while [ "${1:-}" = "--base" ] || [ "${1:-}" = "--from-phase" ]; do
  case "$1" in --base) BASEREF="$2";; --from-phase) FROM="$2";; esac; shift 2
done
MS="${1:?usage: run-aplus.sh [--base REF] [--from-phase 2] <milestone> [arch-budget] [impl-budget]}"
B_ARCH="${2:-12}"; B_IMPL="${3:-30}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
P="$ROOT/harness/prompts"; SCOPE="$P/scope-m$MS.md"
BRANCH="impl/m$MS-aplus"; WT="$(dirname "$ROOT")/$(basename "$ROOT")-m$MS-aplus"; RUN="$ROOT/runs/aplus-m$MS"
[ -f "$SCOPE" ] || { echo "missing $SCOPE" >&2; exit 2; }
if [ -n "$(find "$ROOT/harness/_staged/m$MS" -type f ! -name '.*' 2>/dev/null)" ]; then echo "milestone $MS tests still staged" >&2; exit 2; fi
[ -z "$(git -C "$ROOT" status --porcelain)" ] || { echo "main checkout is dirty; commit first" >&2; exit 2; }
mkdir -p "$RUN"

render() { python3 - "$@" <<'PY'
import sys
tpl, out, *pairs = sys.argv[1:]
s = open(tpl).read()
for kv in pairs:
    k, v = kv.split("=", 1)
    if v.startswith("@"): v = open(v[1:]).read().strip()
    s = s.replace("{{" + k + "}}", v)
open(out, "w").write(s)
PY
}
agent() { # name role budget
  local out="$RUN/$1"; date -u +%Y-%m-%dT%H:%M:%SZ > "$out/started_at"
  set +e
  (cd "$WT" && FACTORY_ROLE="$2" "${AWAKE[@]}" claude -p "$(cat "$out/prompt.md")" --permission-mode auto \
     --output-format stream-json --verbose --max-budget-usd "$3" -n "aplus-m$MS-$1" > "$out/transcript.jsonl" 2> "$out/stderr.log")
  echo "exit=$?" > "$out/exit_code"; set -e
  date -u +%Y-%m-%dT%H:%M:%SZ > "$out/finished_at"
  tail -1 "$out/transcript.jsonl" | jq -r --arg n "$1" '"[\($n)] \(.subtype) turns=\(.num_turns) cost=$\(.total_cost_usd) \((.duration_ms/60000)|floor)min"' 2>/dev/null || true
  ! tail -1 "$out/transcript.jsonl" | jq -e '.result // "" | startswith("API Error") or startswith("You'"'"'ve hit")' >/dev/null 2>&1
}

if [ "$FROM" = 1 ]; then
  [ ! -e "$WT" ] || { echo "$WT exists (use --from-phase 2)" >&2; exit 2; }
  git -C "$ROOT" worktree add -q "$WT" -b "$BRANCH" "$BASEREF"
  (cd "$WT/e2e" && npm ci --no-audit --no-fund >/dev/null)
  mkdir -p "$RUN/architect"; date -u +%Y-%m-%dT%H:%M:%SZ > "$RUN/started_at"
  render "$P/aplus-architect.md" "$RUN/architect/prompt.md" MS="$MS" BRANCH="$BRANCH" SCOPE="@$SCOPE"
  echo "== phase 1: architect"; agent architect architect "$B_ARCH" || { echo "architect failed" >&2; exit 1; }
fi
[ -z "$(git -C "$WT" status --porcelain)" ] || { echo "architect left uncommitted changes" >&2; exit 1; }
[ -f "$WT/spec/plans/m$MS-risks.md" ] || { echo "architect wrote no risk review" >&2; exit 1; }
ARCH_HEAD="$(git -C "$WT" rev-parse HEAD)"; echo "$ARCH_HEAD" > "$RUN/arch_head"

echo "== phase 2: implementer"
rm -rf "$RUN/implementer"; mkdir -p "$RUN/implementer"
render "$P/aplus-implementer.md" "$RUN/implementer/prompt.md" MS="$MS" BRANCH="$BRANCH" SCOPE="@$SCOPE"
sed -i '' "s#integrity --base main --role implementer#integrity --base $ARCH_HEAD --role implementer#" "$RUN/implementer/prompt.md" 2>/dev/null || \
  sed -i "s#integrity --base main --role implementer#integrity --base $ARCH_HEAD --role implementer#" "$RUN/implementer/prompt.md"
agent implementer implementer "$B_IMPL" || echo "implementer ended with an error" >&2
date -u +%Y-%m-%dT%H:%M:%SZ > "$RUN/finished_at"
{ "$ROOT/harness/factory" integrity --root "$WT" --base "$BASEREF" --head "$ARCH_HEAD" --role architect
  "$ROOT/harness/factory" integrity --root "$WT" --base "$ARCH_HEAD" --role implementer; } 2>&1 | tee "$RUN/integrity.txt" || true
echo "worktree: $WT   branch: $BRANCH   run: $RUN"
