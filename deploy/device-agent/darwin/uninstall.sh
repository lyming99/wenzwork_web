#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
common="$script_dir/lib/common.sh"
[[ -f $common ]] || common="$script_dir/../lib/common.sh"
# shellcheck source=../lib/common.sh
source "$common"

confirmed=false
purge=false
purge_confirmation=''
while (($#)); do
  case "$1" in
    --confirm) confirmed=true; shift ;;
    --purge) purge=true; shift ;;
    --confirm-purge) purge_confirmation=${2:-}; shift 2 ;;
    *) agent_die "unknown argument: $1" ;;
  esac
done

agent_require_root
[[ $(agent_host_platform) == darwin ]] || agent_die "this uninstall requires macOS"
if [[ $confirmed == false ]]; then
  [[ -t 0 ]] || agent_die "pass --confirm for non-interactive uninstall"
  read -r -p 'Enter UNINSTALL to remove the Device Agent service and binaries: ' answer
  [[ $answer == UNINSTALL ]] || agent_die "uninstall cancelled"
fi
if [[ $purge == true && $purge_confirmation != DELETE_DEVICE_AGENT_DATA ]]; then
  agent_die "purge requires --confirm-purge DELETE_DEVICE_AGENT_DATA"
fi

config_root=$(agent_validate_config_root '/Library/Application Support/WenzWork/DeviceAgent/config')
install_root=$(agent_installed_root "$config_root/install.conf" /usr/local/lib/wenzwork-device-agent)
launchctl bootout system/com.wenzwork.device-agent >/dev/null 2>&1 || true
rm -f -- /Library/LaunchDaemons/com.wenzwork.device-agent.plist
rm -rf -- "$install_root"

if [[ $purge == true ]]; then
  application_root=$(agent_validate_absolute_root 'Device Agent application root' '/Library/Application Support/WenzWork/DeviceAgent')
  rm -rf -- "$application_root"
  dscl . -delete /Users/_wenzworkagent >/dev/null 2>&1 || true
  dscl . -delete /Groups/_wenzworkagent >/dev/null 2>&1 || true
  agent_log "service, binaries, configuration, identity, secrets, backups, and business data were permanently removed"
else
  agent_log "service and binaries were removed; configuration and all business data remain under /Library/Application Support/WenzWork/DeviceAgent"
fi
