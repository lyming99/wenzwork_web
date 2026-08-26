#!/usr/bin/env bash

set -Eeuo pipefail

PACKAGE_ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=lib/common.sh
source "$PACKAGE_ROOT/runtime/lib/common.sh"

pid_file="$PACKAGE_ROOT/runtime/pids/wenzwork.pid"
[[ -f "$pid_file" ]] || {
  package_log "No package-managed process is running."
  exit 0
}
pid="$(tr -d '[:space:]' < "$pid_file")"
[[ "$pid" =~ ^[1-9][0-9]*$ ]] || package_die "invalid PID file: $pid_file"
if ! kill -0 "$pid" 2>/dev/null; then
  rm -f -- "$pid_file"
  package_log "Removed stale PID file."
  exit 0
fi

command_line="$(ps -p "$pid" -o command= 2>/dev/null || true)"
[[ "$command_line" == *"$PACKAGE_ROOT/bin/"* || "$command_line" == *"$PACKAGE_ROOT/bin\\"* ]] ||
  package_die "PID $pid does not belong to this package"
kill -TERM "$pid"
for _ in {1..30}; do
  kill -0 "$pid" 2>/dev/null || break
  sleep 1
done
kill -0 "$pid" 2>/dev/null && package_die "PID $pid did not stop within 30 seconds"
rm -f -- "$pid_file"
package_log "Package-managed process stopped."
