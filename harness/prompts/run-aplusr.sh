#!/usr/bin/env bash
# Approach A+R (optionally A+/S): architect -> implementer -> {2 reviewers, red team}
# -> fix round -> integrity per role.
#
#   harness/prompts/run-aplusr.sh [--base REF] [--impl-model M] [--from-phase N] <milestone>
#
# Phases: 1 architect (default model), 2 implementer (--impl-model), 3 review +
# red team, 4 fix round (--impl-model). Branch impl/m<N>-aplusr, worktree
# ../<repo>-m<N>-aplusr, run dir runs/aplusr-m<N>/. Heads per phase are kept in
# the run dir so each commit range is checked against the role that made it.
set -euo pipefail
AWAKE=(); command -v caffeinate >/dev/null && AWAKE=(caffeinate -ims)
BASEREF=main; FROM=1; IMODEL=""
while [ $# -gt 1 ] && [[ "$1" == --* ]]; do
  case "$1" in --base) BASEREF="$2";; --from-phase) FROM="$2";; --impl-model) IMODEL="$2";; *) echo "unknown $1" >&2; exit 2;; esac; shift 2
done
MS="${1:?usage: run-aplusr.sh [--base REF] [--impl-model M] [--from-phase N] <milestone>}"
B_ARCH=12; B_IMPL=30; B_REV=6; B_RT=12; B_FIX=15
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"; P="$ROOT/harness/prompts"; SCOPE="$P/scope-m$MS.md"
BRANCH="impl/m$MS-aplusr"; WT="$(dirname "$ROOT")/$(basename "$ROOT")-m$MS-aplusr"; RUN="$ROOT/runs/aplusr-m$MS"
[ -f "$SCOPE" ] || { echo "missing $SCOPE" >&2; exit 2; }
mkdir -p "$RUN"; [ -n "$IMODEL" ] && echo "$IMODEL" > "$RUN/impl_model"; IMODEL="$(cat "$RUN/impl_model" 2>/dev/null || true)"

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
agent() { # name role budget [model]
  local out="$RUN/$1"; mkdir -p "$out"; date -u +%Y-%m-%dT%H:%M:%SZ > "$out/started_at"
  local m=(); [ -n "${4:-}" ] && m=(--model "$4")
  set +e
  (cd "$WT" && FACTORY_ROLE="$2" "${AWAKE[@]}" claude -p "$(cat "$out/prompt.md")" "${m[@]}" --permission-mode auto \
     --output-format stream-json --verbose --max-budget-usd "$3" -n "aplusr-m$MS-$1" > "$out/transcript.jsonl" 2> "$out/stderr.log")
  echo "exit=$?" > "$out/exit_code"; set -e
  date -u +%Y-%m-%dT%H:%M:%SZ > "$out/finished_at"
  tail -1 "$out/transcript.jsonl" | jq -r --arg n "$1" '"[\($n)] \(.subtype) turns=\(.num_turns) cost=$\(.total_cost_usd) \((.duration_ms/60000)|floor)min"' 2>/dev/null || true
  ! tail -1 "$out/transcript.jsonl" | jq -e '.result // "" | (startswith("API Error") or startswith("You'"'"'ve hit"))' >/dev/null 2>&1
}
head_of() { git -C "$WT" rev-parse HEAD; }
clean() { [ -z "$(git -C "$WT" status --porcelain)" ] || { echo "uncommitted changes after $1" >&2; exit 1; }; }

if [ "$FROM" -le 1 ]; then
  [ -z "$(git -C "$ROOT" status --porcelain)" ] || { echo "main checkout is dirty" >&2; exit 2; }
  [ ! -e "$WT" ] || { echo "$WT exists (use --from-phase)" >&2; exit 2; }
  git -C "$ROOT" worktree add -q "$WT" -b "$BRANCH" "$BASEREF"; echo "$BASEREF" > "$RUN/base"
  (cd "$WT/e2e" && npm ci --no-audit --no-fund >/dev/null)
  date -u +%Y-%m-%dT%H:%M:%SZ > "$RUN/started_at"
  mkdir -p "$RUN/architect"; render "$P/aplus-architect.md" "$RUN/architect/prompt.md" MS="$MS" BRANCH="$BRANCH" SCOPE="@$SCOPE"
  echo "== phase 1: architect"; agent architect architect "$B_ARCH" || { echo "architect failed" >&2; exit 1; }
  clean architect; head_of > "$RUN/head_arch"
fi
ARCH_HEAD="$(cat "$RUN/head_arch")"

if [ "$FROM" -le 2 ]; then
  echo "== phase 2: implementer (${IMODEL:-default model})"
  mkdir -p "$RUN/implementer"; render "$P/aplus-implementer.md" "$RUN/implementer/prompt.md" MS="$MS" BRANCH="$BRANCH" SCOPE="@$SCOPE"
  sed -i '' "s#integrity --base main --role implementer#integrity --base $ARCH_HEAD --role implementer#" "$RUN/implementer/prompt.md"
  agent implementer implementer "$B_IMPL" "$IMODEL" || { echo "implementer failed" >&2; exit 1; }
  clean implementer; head_of > "$RUN/head_impl"
fi

if [ "$FROM" -le 3 ]; then
  echo "== phase 3: 2 reviewers + red team (parallel)"
  for r in 1 2; do mkdir -p "$RUN/reviewer-$r"; render "$P/review-single.md" "$RUN/reviewer-$r/prompt.md" MS="$MS" SCOPE="@$SCOPE"; done
  mkdir -p "$RUN/redteam"; render "$P/redteam.md" "$RUN/redteam/prompt.md" MS="$MS" SCOPE="@$SCOPE"
  agent reviewer-1 reviewer "$B_REV" & p1=$!
  agent reviewer-2 reviewer "$B_REV" & p2=$!
  agent redteam red-team "$B_RT" & p3=$!
  wait $p1 || true; wait $p2 || true; wait $p3 || true
  clean "review/red team"; head_of > "$RUN/head_rt"
  # Assemble findings.
  python3 - "$RUN" "$WT" "$MS" > "$RUN/findings.md" <<'PY'
import json, re, sys, os
run, wt, ms = sys.argv[1:4]
print("### Reviewer findings\n")
for r in ("reviewer-1", "reviewer-2"):
    t = [json.loads(l) for l in open(os.path.join(run, r, "transcript.jsonl")) if '"type":"result"' in l]
    res = t[-1].get("result", "") if t else ""
    m = re.findall(r"```json\s*(\{.*?\})\s*```", res, re.S)
    if not m:
        print(f"- ({r}: no structured output)\n"); continue
    d = json.loads(m[-1])
    print(f"**{r}** scores: {d.get('scores')}\n")
    for x in d.get("defects", []):
        print(f"- [{x.get('severity')}] `{x.get('where')}`: {x.get('what')}. Reproduce: {x.get('how_to_reproduce')}")
    print()
f = os.path.join(wt, "harness", "acceptance", "redteam", f"m{ms}", "FINDINGS.md")
print("### Red-team findings\n")
print(open(f).read() if os.path.exists(f) else "(the red team wrote no FINDINGS.md)")
PY
fi

if [ "$FROM" -le 4 ]; then
  echo "== phase 4: fix round (${IMODEL:-default model})"
  mkdir -p "$RUN/fix"; render "$P/aplus-fix.md" "$RUN/fix/prompt.md" MS="$MS" FINDINGS="@$RUN/findings.md"
  agent fix implementer "$B_FIX" "$IMODEL" || echo "fix round ended with an error" >&2
  head_of > "$RUN/head_fix"
fi
date -u +%Y-%m-%dT%H:%M:%SZ > "$RUN/finished_at"

echo "== integrity per role and commit range"
F="$ROOT/harness/factory"; B="$(cat "$RUN/base")"
{ $F integrity --root "$WT" --base "$B" --head "$ARCH_HEAD" --role architect
  $F integrity --root "$WT" --base "$ARCH_HEAD" --head "$(cat "$RUN/head_impl")" --role implementer
  $F integrity --root "$WT" --base "$(cat "$RUN/head_impl")" --head "$(cat "$RUN/head_rt")" --role red-team
  $F integrity --root "$WT" --base "$(cat "$RUN/head_rt")" --role implementer; } 2>&1 | tee "$RUN/integrity.txt" || true
echo "worktree: $WT   branch: $BRANCH   run: $RUN"
