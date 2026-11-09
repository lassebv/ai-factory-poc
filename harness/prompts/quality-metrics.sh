#!/usr/bin/env bash
# Static and dynamic quality metrics for one implementation tree.
#   harness/prompts/quality-metrics.sh <tree> [label]
# Prints KEY=VALUE lines. Uses gocyclo outside go.mod (go run pkg@version).
set -uo pipefail
T="$(cd "$1" && pwd)"; L="${2:-$(basename "$T")}"
cd "$T"
PKGS="${PKGS:-internal/bt/tracker internal/bt/peer internal/bt/piece internal/bt/storage internal/bt/session internal/bt/choke internal/store internal/api cmd/torrentd}"
prodfiles() { find $PKGS -name '*.go' ! -name '*_test.go' 2>/dev/null; }
testfiles() { find $PKGS -name '*_test.go' 2>/dev/null; }
count() { local n=0; while read -r f; do n=$((n + $(grep -cv '^[[:space:]]*$' "$f"))); done; echo $n; }
echo "label=$L"
echo "prod_loc=$(prodfiles | count)"
echo "test_loc=$(testfiles | count)"
echo "test_funcs=$(find $PKGS -name '*_test.go' -print0 | xargs -0 grep -h '^func Test' | wc -l | tr -d ' ')"
echo "files=$(find $PKGS -name '*.go' ! -name '*_test.go' | wc -l | tr -d ' ')"
echo "goroutine_sites=$(find $PKGS -name '*.go' ! -name '*_test.go' -print0 | xargs -0 grep -ho 'go func\|go [a-zA-Z_.]*(' | wc -l | tr -d ' ')"
echo "mutexes=$(find $PKGS -name '*.go' ! -name '*_test.go' -print0 | xargs -0 grep -ho 'sync\.\(RW\)\?Mutex' | wc -l | tr -d ' ')"
echo "channels=$(find $PKGS -name '*.go' ! -name '*_test.go' -print0 | xargs -0 grep -ho 'make(chan ' | wc -l | tr -d ' ')"
echo "ctx_params=$(find $PKGS -name '*.go' ! -name '*_test.go' -print0 | xargs -0 grep -ho 'ctx context\.Context' | wc -l | tr -d ' ')"
cyc="$(go run github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0 -avg $PKGS 2>/dev/null)"
echo "cyclo_avg=$(echo "$cyc" | awk '/^Average/ {print $2}')"
echo "cyclo_over15=$(echo "$cyc" | awk '$1 ~ /^[0-9]+$/ && $1 > 15' | wc -l | tr -d ' ')"
echo "cyclo_max=$(echo "$cyc" | awk '$1 ~ /^[0-9]+$/ {print $1; exit}')"
cov="$(go test -cover ./internal/bt/... ./internal/api/... 2>/dev/null | grep -o 'coverage: [0-9.]*%' | tr -d 'coverage: %' | awk '{s+=$1; n++} END {if (n) printf "%.1f", s/n}')"
echo "coverage_avg=$cov"
if go test -race -count=1 ./internal/... >/tmp/race.$$ 2>&1; then echo "race=clean"; else echo "race=FAIL"; fi
rm -f /tmp/race.$$
echo "staticcheck_issues=$(go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 -checks all ./internal/... ./cmd/... 2>/dev/null | wc -l | tr -d ' ')"
