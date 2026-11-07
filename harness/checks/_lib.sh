# Shared helpers for harness/checks/*.sh (sourced, not run).
set -euo pipefail
ROOT="${GATE_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
cd "$ROOT"
require_product() {
  if [ ! -f "$ROOT/go.mod" ]; then
    echo "no go.mod at the repo root: the product module does not exist yet" >&2
    exit 1
  fi
}
