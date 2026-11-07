#!/usr/bin/env bash
# Launch an Approach C run (contract-first fan-out) on one milestone:
#   phase 1  architect     freezes contract.go/contract_test.go + writes spec/plans/m<N>.json
#   phase 2  implementers  one headless session per work package, in parallel, own branch + worktree
#   phase 3  integrator    merges the work packages, wires session/API/cmd, drives the gate
#
#   harness/prompts/run-fanout.sh [--from-phase 2|3] <milestone> [arch-budget] [wp-budget] [integrator-budget]
#
# --from-phase 2 reuses a finished architect phase (branch impl/m<N>-c and its
# worktree must exist, with a committed plan) and reruns implementers +
# integrator; existing work-package branches/worktrees are discarded first.
# --from-phase 3 reruns only the integrator on the existing work-package branches.
#
# Branches: impl/m<N>-c (architect + integration), impl/m<N>-c-<wp> (work packages).
# Worktrees: ../<repo>-m<N>c and ../<repo>-m<N>c-<wp>. Transcripts and timing per
# agent in runs/approach-c-m<N>/<agent>/ of the main checkout.
set -euo pipefail
# Keep macOS from idle/system sleeping during a run: a sleeping machine kills
# in-flight responses ("Your computer went to sleep mid-response"). Lid-close
# sleep can't be prevented this way; keep the lid open (or use clamshell on power).
AWAKE=(); command -v caffeinate >/dev/null && AWAKE=(caffeinate -ims)
FROM=1
if [ "${1:-}" = "--from-phase" ]; then FROM="${2:?--from-phase needs 2 or 3}"; shift 2; fi
MS="${1:?usage: run-fanout.sh [--from-phase 2|3] <milestone> [arch-budget] [wp-budget] [integrator-budget]}"
B_ARCH="${2:-10}"; B_WP="${3:-10}"; B_INT="${4:-25}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
P="$ROOT/harness/prompts"
SCOPE="$P/scope-m$MS.md"
BASE="impl/m$MS-c"
WT="$(dirname "$ROOT")/$(basename "$ROOT")-m${MS}c"
RUN="$ROOT/runs/approach-c-m$MS"

[ -f "$SCOPE" ] || { echo "missing $SCOPE" >&2; exit 2; }
if [ -n "$(find "$ROOT/harness/_staged/m$MS" -type f ! -name '.*' 2>/dev/null)" ]; then
  echo "milestone $MS acceptance tests are still staged; promote them first" >&2; exit 2
fi
[ -z "$(git -C "$ROOT" status --porcelain)" ] || { echo "main checkout is dirty; commit first" >&2; exit 2; }
if [ "$FROM" = 1 ]; then
  [ ! -e "$WT" ] || { echo "$WT already exists (use --from-phase 2 to reuse it)" >&2; exit 2; }
else
  [ -d "$WT" ] || { echo "--from-phase $FROM needs the existing worktree $WT" >&2; exit 2; }
  [ -z "$(git -C "$WT" status --porcelain)" ] || { echo "$WT has uncommitted changes" >&2; exit 2; }
fi
mkdir -p "$RUN"

# render TEMPLATE OUT KEY=VALUE... ; a value of @file inserts that file's contents.
render() {
  python3 - "$@" <<'EOF'
import sys
tpl, out, *pairs = sys.argv[1:]
s = open(tpl).read()
for kv in pairs:
    k, v = kv.split("=", 1)
    if v.startswith("@"):
        v = open(v[1:]).read().strip()
    s = s.replace("{{" + k + "}}", v)
open(out, "w").write(s)
EOF
}

# agent NAME DIR ROLE BUDGET : runs claude headless with RUN/NAME/prompt.md
agent() {
  local name="$1" dir="$2" role="$3" budget="$4" out="$RUN/$1"
  date -u +%Y-%m-%dT%H:%M:%SZ > "$out/started_at"
  set +e
  (cd "$dir" && FACTORY_ROLE="$role" "${AWAKE[@]}" claude -p "$(cat "$out/prompt.md")" \
    --permission-mode auto --output-format stream-json --verbose \
    --max-budget-usd "$budget" -n "approach-c-m$MS-$name" \
    > "$out/transcript.jsonl" 2> "$out/stderr.log")
  echo "exit=$?" > "$out/exit_code"
  set -e
  date -u +%Y-%m-%dT%H:%M:%SZ > "$out/finished_at"
  tail -1 "$out/transcript.jsonl" | jq -r --arg n "$name" '"[\($n)] \(.subtype) turns=\(.num_turns) cost=$\(.total_cost_usd) \((.duration_ms/60000)|floor)min"' 2>/dev/null || echo "[$name] no result"
  if tail -1 "$out/transcript.jsonl" | jq -e '.result // "" | startswith("API Error")' >/dev/null 2>&1; then
    echo "[$name] ended with an API error: $(tail -1 "$out/transcript.jsonl" | jq -r .result | head -1)" >&2
    echo "api-error" > "$out/failed"
    return 1
  fi
}

if [ "$FROM" = 1 ]; then
  echo "== phase 1: architect"
  date -u +%Y-%m-%dT%H:%M:%SZ > "$RUN/started_at"
  git -C "$ROOT" worktree add -q "$WT" -b "$BASE" main
  (cd "$WT/e2e" && npm ci --no-audit --no-fund >/dev/null)
  mkdir -p "$RUN/architect"
  render "$P/fanout-architect.md" "$RUN/architect/prompt.md" MS="$MS" BRANCH="$BASE" SCOPE="@$SCOPE"
  agent architect "$WT" architect "$B_ARCH" || { echo "architect failed; stopping" >&2; exit 1; }
else
  echo "== phase 1: reusing the architect's work on $BASE ($(git -C "$WT" log --oneline -1))"
fi

PLAN="$WT/spec/plans/m$MS.json"
[ -f "$PLAN" ] || { echo "architect produced no $PLAN" >&2; exit 1; }
[ -z "$(git -C "$WT" status --porcelain)" ] || { echo "architect left uncommitted changes" >&2; exit 1; }
python3 - "$PLAN" <<'EOF'
import json, re, sys
plan = json.load(open(sys.argv[1]))
wps = plan["workPackages"]
assert 2 <= len(wps) <= 5, "need 2-5 work packages"
seen = {}
for w in wps:
    assert re.fullmatch(r"[a-z0-9-]+", w["id"]), w["id"]
    for p in w["packages"]:
        assert p not in seen, f"package {p} in both {seen[p]} and {w['id']}"
        seen[p] = w["id"]
for p in plan["integration"]["packages"]:
    assert p not in seen, f"integration package {p} also in work package {seen[p]}"
print("plan ok:", ", ".join(w["id"] for w in wps))
EOF
ARCH_HEAD="$(git -C "$WT" rev-parse HEAD)"
WPS=$(jq -r '.workPackages[].id' "$PLAN")

if [ "$FROM" -le 2 ]; then
echo "== phase 2: implementers (parallel)"
pids=()
for id in $WPS; do
  if [ -e "$WT-$id" ] || git -C "$ROOT" rev-parse -q --verify "refs/heads/$BASE-$id" >/dev/null; then
    echo "discarding earlier attempt of $id"
    git -C "$ROOT" worktree remove --force "$WT-$id" 2>/dev/null || true
    git -C "$ROOT" branch -D "$BASE-$id" >/dev/null 2>&1 || true
    rm -rf "$RUN/wp-$id"
  fi
  br="$BASE-$id"; wdir="$WT-$id"
  git -C "$ROOT" worktree add -q "$wdir" -b "$br" "$ARCH_HEAD"
  mkdir -p "$RUN/wp-$id"
  jq -r --arg id "$id" '.workPackages[] | select(.id==$id) | .notes' "$PLAN" > "$RUN/wp-$id/notes.txt"
  render "$P/fanout-implementer.md" "$RUN/wp-$id/prompt.md" MS="$MS" WP_ID="$id" \
    WP_TITLE="$(jq -r --arg id "$id" '.workPackages[] | select(.id==$id) | .title' "$PLAN")" \
    WP_PACKAGES="$(jq -r --arg id "$id" '.workPackages[] | select(.id==$id) | .packages | map("`"+.+"`") | join(", ")' "$PLAN")" \
    WP_NOTES="@$RUN/wp-$id/notes.txt" BRANCH="$br" BASE="$ARCH_HEAD"
  agent "wp-$id" "$wdir" implementer "$B_WP" &
  pids+=($!)
done
for pid in "${pids[@]}"; do wait "$pid" || true; done
fi
for id in $WPS; do
  [ ! -f "$RUN/wp-$id/failed" ] || { echo "work package $id ended with an API error; stopping before integration" >&2; exit 1; }
done

echo "== phase 3: integrator"
rm -rf "$RUN/integrator"; mkdir -p "$RUN/integrator"
branches=$(for id in $WPS; do printf '`%s-%s` ' "$BASE" "$id"; done)
render "$P/fanout-integrator.md" "$RUN/integrator/prompt.md" MS="$MS" BRANCH="$BASE" SCOPE="@$SCOPE" \
  N_WP="$(echo "$WPS" | wc -w | tr -d ' ')" WP_BRANCHES="$branches" ARCH_HEAD="$ARCH_HEAD"
agent integrator "$WT" implementer "$B_INT" || echo "integrator ended with an API error" >&2
date -u +%Y-%m-%dT%H:%M:%SZ > "$RUN/finished_at"

echo "== integrity per role"
{ "$ROOT/harness/factory" integrity --root "$WT" --base main --head "$ARCH_HEAD" --role architect
  "$ROOT/harness/factory" integrity --root "$WT" --base "$ARCH_HEAD" --role implementer
} 2>&1 | tee "$RUN/integrity.txt" || true

python3 - "$RUN" <<'EOF'
import json, os, sys, glob
run = sys.argv[1]; out = {}
for d in sorted(glob.glob(os.path.join(run, "*", "transcript.jsonl"))):
    name = os.path.basename(os.path.dirname(d))
    res = [json.loads(l) for l in open(d) if '"type":"result"' in l]
    r = res[-1] if res else {}
    out[name] = {k: r.get(k) for k in ("subtype", "num_turns", "total_cost_usd", "duration_ms")}
json.dump(out, open(os.path.join(run, "summary.json"), "w"), indent=2)
print(json.dumps(out, indent=2))
EOF
echo "integration worktree: $WT   branch: $BASE   run: $RUN"
