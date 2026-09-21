#!/usr/bin/env bash
set -euo pipefail
# Usage: bash scripts/test-main-affinity.sh /path/to/unmodified/CPA/main
# A Go overlay adds only a test file; no CPA source or configuration is edited.
plugin_root=$(cd "$(dirname "$0")/.." && pwd)
cpa_root=$(cd "${1:?CPA main checkout required}" && pwd)
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
make -C "$plugin_root" build PLUGIN_OUTPUT="$test_dir/codex-quota-scheduler.so"
python3 - "$plugin_root" "$cpa_root" "$test_dir" <<'PY'
import json,sys
from pathlib import Path
plugin,cpa,temp=map(Path,sys.argv[1:])
overlay={"Replace":{str(cpa/'internal/pluginhost/quota_scheduler_main_compat_test.go'):str(plugin/'tests/main_host_affinity_test.go.txt')}}
(temp/'overlay.json').write_text(json.dumps(overlay))
PY
cd "$cpa_root"
XDG_CONFIG_HOME="$test_dir/config" CPA_RESET_POLICY_FILE="" CPA_QUOTA_PLUGIN_LIBRARY="$test_dir/codex-quota-scheduler.so" go test -overlay="$test_dir/overlay.json" ./internal/pluginhost -run '^TestQuotaSchedulerStandaloneOnMain$' -count=1 -timeout=90s
