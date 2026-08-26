#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
common="$script_dir/lib/common.sh"
[[ -f $common ]] || common="$script_dir/../lib/common.sh"
# shellcheck source=../lib/common.sh
source "$common"

wait_seconds=0
while (($#)); do
  case "$1" in
    --wait) wait_seconds=${2:-0}; shift 2 ;;
    *) agent_die "unknown argument: $1" ;;
  esac
done
[[ $wait_seconds =~ ^[0-9]+$ ]] || agent_die "invalid --wait value"
[[ $(agent_host_platform) == darwin ]] || agent_die "this health check requires macOS"

config_root='/Library/Application Support/WenzWork/DeviceAgent/config'
install_root=${WENZWORK_DEVICE_AGENT_INSTALL_ROOT:-$(agent_installed_root "$config_root/install.conf" /usr/local/lib/wenzwork-device-agent)}
install_root=$(agent_validate_install_root "$install_root")
deadline=$((SECONDS + wait_seconds))
while true; do
  current_version=''
  binary_version=''
  if [[ -L $install_root/current && -f $install_root/current/VERSION && -x $install_root/current/bin/wenzwork-device-agent ]]; then
    current_version=$(tr -d '[:space:]' < "$install_root/current/VERSION")
    binary_version=$("$install_root/current/bin/wenzwork-device-agent" version 2>/dev/null || true)
  fi
  if [[ -n $current_version && $binary_version == "$current_version" ]] &&
    launchctl print system/com.wenzwork.device-agent 2>/dev/null | grep -Eq 'state = running|pid = [1-9][0-9]*'; then
    sleep 2
    launchctl print system/com.wenzwork.device-agent 2>/dev/null | grep -Eq 'state = running|pid = [1-9][0-9]*' && exit 0
  fi
  (( SECONDS < deadline )) || exit 1
  sleep 1
done
