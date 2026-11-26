#!/bin/sh
set -eu
cd "$(dirname "$0")"
if [ -f ./.env ]; then
  set -a
  . ./.env
  set +a
fi
: "${ADMIN_TOKEN:?set ADMIN_TOKEN before starting}"
export PATH="$PATH:/usr/local/go/bin"

if [ ! -x ./status-monitor ] || { command -v go >/dev/null 2>&1 && [ -n "$(find app -newer ./status-monitor -type f -print -quit)" ]; }; then
  if ! command -v go >/dev/null 2>&1; then
    printf '%s\n' 'Go is unavailable and status-monitor has not been built.' >&2
    exit 1
  fi
  build_dir=$(mktemp -d ./.build.XXXXXX)
  trap 'rm -rf "$build_dir"' EXIT HUP INT TERM
  (cd app && go build -trimpath -o "../$build_dir/status-monitor" .)
  mv "$build_dir/status-monitor" ./status-monitor
  rmdir "$build_dir"
  trap - EXIT HUP INT TERM
fi

set -- -config "${STATUS_CONFIG:-config.yaml}"
if [ -n "${ADDR:-}" ]; then
  set -- "$@" -addr "$ADDR"
fi
exec ./status-monitor "$@"
