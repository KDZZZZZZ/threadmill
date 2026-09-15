#!/bin/sh
set -eu

root=${THREADMILL_ROOT:-/workspace}
config=${THREADMILL_CONFIG:-}
if [ ! -d "$root" ]; then
  echo "threadmill: project root does not exist: $root" >&2
  exit 64
fi
if [ -n "$config" ]; then
  /usr/local/bin/threadmill -check -C "$root" -config "$config"
else
  /usr/local/bin/threadmill -check -C "$root"
fi
exec /usr/local/bin/threadmill -C "$root" "$@"
