#!/usr/bin/env bash
# AC-13: CGO_ENABLED=0 builds for all four targets, each < 40 MB, and the
# native binary runs on its own from an empty directory.
source "$(dirname "$0")/_lib.sh"
require_product
LIMIT=40000000
OUT="$(mktemp -d)"
trap 'kill "${PID:-}" 2>/dev/null || true; rm -rf "$OUT"' EXIT
status=0
for target in linux/amd64 linux/arm64 darwin/arm64 windows/amd64; do
  os="${target%/*}"; arch="${target#*/}"
  bin="$OUT/torrentd-$os-$arch"; [ "$os" = windows ] && bin="$bin.exe"
  if ! CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -o "$bin" ./cmd/torrentd; then
    echo "FAIL build $target"; status=1; continue
  fi
  size=$(wc -c < "$bin" | tr -d ' ')
  if [ "$size" -ge "$LIMIT" ]; then echo "FAIL $target is $size bytes (limit $LIMIT)"; status=1
  else echo "ok   $target $size bytes"; fi
done

# Run the native build from an empty directory with an empty HOME.
native="$OUT/torrentd-$(go env GOOS)-$(go env GOARCH)"
RUN="$OUT/run"; mkdir -p "$RUN/home"
cp "$native" "$RUN/torrentd"
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
( cd "$RUN" && HOME="$RUN/home" XDG_DATA_HOME= ./torrentd --addr "127.0.0.1:$PORT" --peer-port 0 >"$RUN/log" 2>&1 ) &
PID=$!
for _ in $(seq 1 150); do
  if curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
    echo "ok   native binary runs standalone from an empty directory"
    exit $status
  fi
  sleep 0.1
done
echo "FAIL native binary did not answer /healthz within 15 s"; tail -20 "$RUN/log"
exit 1
