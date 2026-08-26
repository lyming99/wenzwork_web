#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

purge=false
confirmation=''
install_root=''
while (($#)); do
  case "$1" in
    --purge) purge=true; shift ;;
    --confirm-purge) confirmation=${2:-}; shift 2 ;;
    --install-root|--work-dir) install_root=${2:-}; shift 2 ;;
    *) relay_die "unknown argument: $1" ;;
  esac
done
relay_require_root
[[ -n $install_root ]] || install_root=$(relay_installed_root)
install_root=$(relay_validate_install_root "$install_root")

if [[ $purge == true ]]; then
  if [[ -f /etc/wenzwork-relay/config.yaml ]]; then
    installation_id=$(awk '$1 == "installation_id:" {print $2; exit}' /etc/wenzwork-relay/config.yaml)
    [[ -n $installation_id && $confirmation == "$installation_id" ]] ||
      relay_die "purge requires --confirm-purge <installation-id>"
  elif [[ -f /etc/wenzwork-relay/relay.env ]]; then
    [[ $confirmation == DELETE_RELAY_DATA ]] ||
      relay_die "Access Key mode purge requires --confirm-purge DELETE_RELAY_DATA"
  else
    relay_die "Relay configuration is already absent"
  fi
fi

systemctl disable --now wenzwork-relay.service 2>/dev/null || true
rm -f -- /etc/systemd/system/wenzwork-relay.service
systemctl daemon-reload
if [[ -L /usr/local/bin/relayctl && $(readlink /usr/local/bin/relayctl) == "$install_root/current/bin/relayctl" ]]; then
  rm -f -- /usr/local/bin/relayctl
fi
rm -rf -- "$install_root"

if [[ $purge == true ]]; then
  rm -rf -- /etc/wenzwork-relay /var/lib/wenzwork-relay
  userdel wenzwork-relay 2>/dev/null || true
  groupdel wenzwork-relay 2>/dev/null || true
  relay_log "Relay binaries, environment, configuration, certificates, and identity were permanently removed"
else
  relay_log "Relay binaries were removed from $install_root; configuration and identity remain under /etc/wenzwork-relay and /var/lib/wenzwork-relay"
fi
