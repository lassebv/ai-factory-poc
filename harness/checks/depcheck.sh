#!/usr/bin/env bash
# AC-02: the real module graph passes tools/depcheck (§2).
source "$(dirname "$0")/_lib.sh"
require_product
go run ./tools/depcheck .
