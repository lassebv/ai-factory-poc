#!/usr/bin/env bash
# Regenerate synthetic fixtures and expected.json from the independent oracles.
# Requires Docker. Run from anywhere.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
HARNESS="$(cd "$HERE/../.." && pwd)"
python3 "$HERE/synth/generate.py"
docker build -q -t torrentd-oracle:1 "$HARNESS/oracle" >/dev/null
docker run --rm -v "$HARNESS:/h" torrentd-oracle:1 sh -c \
  'python3 /h/oracle/describe.py /h/fixtures/torrents/real/*.torrent /h/fixtures/torrents/libtorrent/*.torrent /h/fixtures/torrents/synth/*.torrent' \
  > "$HERE/oracle.jsonl"
python3 "$HERE/build_expected.py"
