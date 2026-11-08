#!/usr/bin/env bash
# RACE-01: the whole product test suite passes under the race detector (§4.5).
source "$(dirname "$0")/_lib.sh"
require_product
go test -race -count=1 ./...
