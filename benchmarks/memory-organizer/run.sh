#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 3 ]]; then
  echo "usage: $0 MEMORY_JSON [SOURCE_ENV] [OUTPUT_DIR]" >&2
  exit 2
fi

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$script_dir/../.." && pwd)
memory_json=$1
source_env=${2:-env-30}
run_stamp=$(date -u +%Y%m%dT%H%M%SZ)
output_dir=${3:-/tmp/threadmill-memory-organizer/$run_stamp}
config_root=${THREADMILL_CONFIG_ROOT:-$output_dir/config-root}
timeout=${THREADMILL_ORGANIZER_TIMEOUT:-20m}
subscriptions=${THREADMILL_ORGANIZER_SUBSCRIPTIONS:-sg-q-1,sg-q-2}
session_reset=${THREADMILL_ORGANIZER_SESSION_RESET:-true}
attribution=${THREADMILL_SUBSCRIPTION_ATTRIBUTION:-false}

config_args=()
if [[ -n ${THREADMILL_CONFIG_PATH:-} ]]; then
  config_args+=(--config "$THREADMILL_CONFIG_PATH")
fi

cd "$repo_root"
mkdir -p "$config_root"
go test ./benchmarks/memory-organizer -count=1
go run ./benchmarks/memory-organizer \
  --memory "$memory_json" \
  --env "$source_env" \
  --queries "$script_dir/queries.json" \
  --out "$output_dir" \
  --config-root "$config_root" \
  --initial-subscriptions "$subscriptions" \
  --session-reset="$session_reset" \
  --subscription-attribution="$attribution" \
  --timeout "$timeout" \
  "${config_args[@]}"

echo "artifacts=$output_dir"
