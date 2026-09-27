#!/usr/bin/env bash
set -euo pipefail

# The spec stays in the planning project; do not create a second source of truth.
spec_path="$(cd "$(dirname "${1:?Usage: verify_workbench.sh /path/to/prd.json}")" && pwd)/$(basename "$1")"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bun_bin="${BUN:-bun}"
bun_bin="$(command -v "$bun_bin")"
export PATH="$(dirname "$bun_bin"):$PATH"
report_dir="${WORKBENCH_REPORT_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/workbench-acceptance.XXXXXX")}"
mkdir -p "$report_dir"
report_dir="$(cd "$report_dir" && pwd)"

cd "$repo_root/apps/web"
"$bun_bin" install --frozen-lockfile
"$bun_bin" run test --reporter=json --outputFile="$report_dir/vitest.json"
"$bun_bin" run build

docker exec paca-assistant-api-1 go test ./internal/service/draft ./internal/service/notification ./internal/transport/http/handler ./internal/worker
docker exec paca-assistant-agent-runner-1 go test ./internal/acpbridge
docker run --rm -v "$repo_root/apps/acp-bridge:/workspace:ro" \
  -v paca-dev_go_build_cache:/root/.cache/go-build -w /workspace -e CGO_ENABLED=0 \
  paca-dev-api:latest go test ./internal/runner ./internal/bridge

cd "$repo_root/apps/e2e"
"$bun_bin" install --frozen-lockfile
cd "$repo_root/services/realtime"
"$bun_bin" install --frozen-lockfile
cd "$repo_root"
python3 scripts/ralph/test_validate_spec.py
python3 scripts/ralph/validate_spec.py --spec "$spec_path" --all | tee "$report_dir/scenarios.json"

git diff --check
printf 'Workbench acceptance passed. Results: %s\n' "$report_dir"
