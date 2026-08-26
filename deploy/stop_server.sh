#!/usr/bin/env bash

set -Eeuo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
START_SCRIPT="$SCRIPT_DIR/start_server.sh"

usage() {
  cat <<'EOF'
Usage: ./stop_server.sh

Gracefully stops the WenzWork API process recorded in run/wenzwork-api.pid.
If it does not exit within WENZWORK_STOP_TIMEOUT seconds, the process is
forcefully stopped. The PID is verified before any signal is sent.
EOF
}

case "${1:-}" in
  -h | --help | help)
    usage
    exit 0
    ;;
  "")
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

[[ -x "$START_SCRIPT" ]] || {
  printf '[wenzwork-stop] ERROR: start_server.sh is missing or not executable: %s\n' "$START_SCRIPT" >&2
  exit 1
}

exec "$START_SCRIPT" stop
