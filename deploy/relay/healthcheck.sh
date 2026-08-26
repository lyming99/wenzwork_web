#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

mode=ready
wait_seconds=0
diagnostics=''
while (($#)); do
  case "$1" in
    --live) mode=live; shift ;;
    --ready) mode=ready; shift ;;
    --wait) wait_seconds=${2:-0}; shift 2 ;;
    --diagnostics) diagnostics=${2:-}; shift 2 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
[[ $wait_seconds =~ ^[0-9]+$ ]] || { printf 'invalid --wait value\n' >&2; exit 2; }

env_file=${RELAY_ENV_FILE:-/etc/wenzwork-relay/relay.env}
config_file=${WENZWORK_RELAY_CONFIG:-/etc/wenzwork-relay/config.yaml}
install_root=${RELAY_INSTALL_ROOT:-$(relay_installed_root)}
install_root=$(relay_validate_install_root "$install_root")
if [[ -f $env_file ]]; then
  [[ $(stat -c '%a' "$env_file") == 600 || $(stat -c '%a' "$env_file") == 640 ]]
  grep -Eq '^RELAY_ACCESS_KEY=relay_[A-Za-z0-9_-]{43}$' "$env_file"
  health_address=127.0.0.1:19090
else
  "$install_root/current/bin/relayctl" config check --config-file "$config_file" >/dev/null
  [[ $(stat -c '%a' /var/lib/wenzwork-relay/identity/identity.key) == 600 ]]
  health_address=$(awk -F': ' '$1 == "health_address" {print $2; exit}' "$config_file" | tr -d "\"'")
  [[ -n $health_address ]] || { printf 'health_address is missing from Relay config\n' >&2; exit 1; }
fi
endpoint="http://$health_address/health/$mode"
deadline=$((SECONDS + wait_seconds))
until curl --fail --silent --show-error --max-time 3 "$endpoint"; do
  (( SECONDS < deadline )) || exit 1
  sleep 1
done

if [[ -n $diagnostics ]]; then
  [[ $diagnostics != / && $diagnostics != /etc && $diagnostics != /var && $diagnostics != /opt ]] || exit 2
  work_dir=$(mktemp -d /tmp/wenzwork-relay-diagnostics.XXXXXX)
  cleanup() {
    relay_remove_temp_tree "$work_dir"
  }
  trap cleanup EXIT
  curl --silent --show-error --max-time 3 "http://$health_address/status" > "$work_dir/status.json"
  systemctl show wenzwork-relay.service \
    --property=ActiveState,SubState,ExecMainStatus,NRestarts,MemoryCurrent > "$work_dir/systemd.txt"
  journalctl -u wenzwork-relay.service --since '-30 minutes' --no-pager -n 1000 |
    sed -E 's/([Tt]oken|[Tt]icket|[Aa]uthorization|[Aa]ccess[_ -]?[Kk]ey|RELAY_ACCESS_KEY|[Pp]rivate[_ -]?[Kk]ey)[=: ]+[^ ]+/\1=[REDACTED]/g' > "$work_dir/journal.txt"
  tar -C "$work_dir" -czf "$diagnostics" status.json systemd.txt journal.txt
  chmod 0600 "$diagnostics"
fi
