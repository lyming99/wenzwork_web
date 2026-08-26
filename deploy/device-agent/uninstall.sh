#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

install_root=''
confirmed=false
purge=false
purge_confirmation=''
while (($#)); do
  case "$1" in
    --install-root) install_root=${2:-}; shift 2 ;;
    --confirm) confirmed=true; shift ;;
    --purge) purge=true; shift ;;
    --confirm-purge) purge_confirmation=${2:-}; shift 2 ;;
    *) agent_die "unknown argument: $1" ;;
  esac
done

agent_require_root
[[ $(agent_host_platform) == linux ]] || agent_die "this uninstall requires Linux"
[[ -n $install_root ]] || install_root=$(agent_installed_root /etc/wenzwork-device-agent/install.conf /opt/wenzwork-device-agent)
install_root=$(agent_validate_install_root "$install_root")
if [[ $confirmed == false ]]; then
  [[ -t 0 ]] || agent_die "pass --confirm for non-interactive uninstall"
  read -r -p 'Enter UNINSTALL to remove the Device Agent service and binaries: ' answer
  [[ $answer == UNINSTALL ]] || agent_die "uninstall cancelled"
fi
if [[ $purge == true && $purge_confirmation != DELETE_DEVICE_AGENT_DATA ]]; then
  agent_die "purge requires --confirm-purge DELETE_DEVICE_AGENT_DATA"
fi

systemctl disable --now wenzwork-device-agent.service 2>/dev/null || true
rm -f -- /etc/systemd/system/wenzwork-device-agent.service
systemctl daemon-reload
rm -rf -- "$install_root"

if [[ $purge == true ]]; then
  config_root=$(agent_validate_config_root /etc/wenzwork-device-agent)
  data_root=$(agent_validate_data_root /var/lib/wenzwork-device-agent)
  backup_root=$(agent_validate_absolute_root 'Device Agent backup root' /var/backups/wenzwork-device-agent)
  rm -rf -- "$config_root" "$data_root" "$backup_root"
  userdel wenzwork-agent 2>/dev/null || true
  groupdel wenzwork-agent 2>/dev/null || true
  agent_log "service, binaries, configuration, identity, secrets, backups, and business data were permanently removed"
else
  agent_log "service and binaries were removed; configuration remains in /etc/wenzwork-device-agent and all business data remains in /var/lib/wenzwork-device-agent"
fi
