#!/usr/bin/env bash
# LINT-01: no direct wall-clock use in engine packages (T1).
source "$(dirname "$0")/_lib.sh"
exec "$ROOT/harness/factory" lint-clock --root "$ROOT"
