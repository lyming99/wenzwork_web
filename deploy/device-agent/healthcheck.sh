#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

wait_seconds=0
while (($#)); do
  case "$1" in
    --wait) wait_seconds=${2:-0}; shift 2 ;;
    *) agent_die "unknown argument: $1" ;;
  esac
done
[[ $wait_seconds =~ ^[0-9]+$ ]] || agent_die "invalid --wait value"
[[ $(agent_host_platform) == linux ]] || agent_die "this health check requires Linux"

install_root=${WENZWORK_DEVICE_AGENT_INSTALL_ROOT:-$(agent_installed_root /etc/wenzwork-device-agent/install.conf /opt/wenzwork-device-agent)}
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
    systemctl is-active --quiet wenzwork-device-agent.service &&
    [[ $(systemctl show wenzwork-device-agent.service --property=SubState --value) == running ]]; then
    # A short stability window catches immediate crash/restart loops while still
    # allowing the Agent's own reconnect loop to handle an offline Control Plane.
    sleep 2
    systemctl is-active --quiet wenzwork-device-agent.service && exit 0
  fi
  (( SECONDS < deadline )) || exit 1
  sleep 1
done
