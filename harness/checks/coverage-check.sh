#!/usr/bin/env bash
# QUAL-01: coverage floors per package (ADR-0009).
source "$(dirname "$0")/_lib.sh"
require_product
exec "$ROOT/harness/factory" coverage --root "$ROOT"
