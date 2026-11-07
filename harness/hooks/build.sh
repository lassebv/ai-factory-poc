#!/usr/bin/env bash
# Build harness/bin/factory if it is missing or older than its sources.
# Prints nothing on success; prints the build error and exits 1 on failure.
set -u
HARNESS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$HARNESS/bin/factory"
if [ -x "$BIN" ] && [ -z "$(find "$HARNESS/cmd" "$HARNESS/internal" "$HARNESS/go.mod" "$HARNESS/go.sum" -newer "$BIN" -print -quit 2>/dev/null)" ]; then
  exit 0
fi
mkdir -p "$HARNESS/bin"
TMP="$HARNESS/bin/.factory.$$"
if ! out="$(cd "$HARNESS" && go build -o "$TMP" ./cmd/factory 2>&1)"; then
  rm -f "$TMP"; echo "$out"; exit 1
fi
mv -f "$TMP" "$BIN"   # atomic, so concurrent agents never run a half-written binary
