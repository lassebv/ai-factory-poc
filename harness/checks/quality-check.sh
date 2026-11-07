#!/usr/bin/env bash
# QUAL-02: go vet, staticcheck, doc comments, file length (ADR-0009).
source "$(dirname "$0")/_lib.sh"
require_product
STATICCHECK_VERSION=2025.1.1
status=0
echo "== go vet"; go vet ./... || status=1
echo "== staticcheck $STATICCHECK_VERSION"
go run "honnef.co/go/tools/cmd/staticcheck@$STATICCHECK_VERSION" ./... || status=1
echo "== doc comments and file length"
"$ROOT/harness/factory" quality --root "$ROOT" || status=1
exit $status
