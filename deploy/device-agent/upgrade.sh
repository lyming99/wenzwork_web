#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

install_root=''
archive=''
archive_url=''
checksums=''
checksums_url=''
signature=''
signature_url=''
signing_key=''
confirmed=false
while (($#)); do
  case "$1" in
    --install-root) install_root=${2:-}; shift 2 ;;
    --package-file) archive=${2:-}; shift 2 ;;
    --artifact-url) archive_url=${2:-}; shift 2 ;;
    --checksums-file) checksums=${2:-}; shift 2 ;;
    --checksums-url) checksums_url=${2:-}; shift 2 ;;
    --checksums-signature-file) signature=${2:-}; shift 2 ;;
    --checksums-signature-url) signature_url=${2:-}; shift 2 ;;
    --signing-key-file) signing_key=${2:-}; shift 2 ;;
    --confirm-upgrade) confirmed=true; shift ;;
    *) agent_die "unknown argument: $1" ;;
  esac
done

agent_require_root
[[ $(agent_host_platform) == linux ]] || agent_die "this upgrade requires Linux"
[[ -n $install_root ]] || install_root=$(agent_installed_root /etc/wenzwork-device-agent/install.conf /opt/wenzwork-device-agent)
install_root=$(agent_validate_install_root "$install_root")
[[ -L $install_root/current ]] || agent_die "current release link is missing"
[[ $confirmed == true ]] || {
  [[ -t 0 ]] || agent_die "pass --confirm-upgrade for non-interactive upgrade"
  read -r -p 'Enter UPGRADE to stop the Agent, back up its data, and continue: ' answer
  [[ $answer == UPGRADE ]] || agent_die "upgrade cancelled"
}

data_root=$(agent_validate_data_root /var/lib/wenzwork-device-agent)
agent_assert_atomic_restore_layout "$data_root"
config_root=$(agent_validate_config_root /etc/wenzwork-device-agent)
env_file="$config_root/agent.env"
backup_root=$(agent_validate_absolute_root 'Device Agent backup root' /var/backups/wenzwork-device-agent)
expected_state="$data_root/state/agent-state.json"
agent_validate_env_file "$env_file" "$expected_state"
[[ -n $signing_key ]] || signing_key="$install_root/current/release-signing-public-key.pem"

work_dir=$(mktemp -d /tmp/wenzwork-device-agent-upgrade.XXXXXX)
# shellcheck disable=SC2329 # Invoked indirectly by trap.
cleanup() { agent_remove_temp_tree "$work_dir"; }
trap cleanup EXIT HUP INT TERM
package_info=$(agent_prepare_package "$archive" "$archive_url" "$checksums" "$checksums_url" \
  "$signature" "$signature_url" "$signing_key" '' "$work_dir")
package_root=${package_info%%$'\n'*}
version=${package_info#*$'\n'}

previous_target=$(readlink "$install_root/current")
previous_release=$(cd "$install_root/current" && pwd -P)
[[ $previous_release == "$install_root"/releases/* && -f $previous_release/VERSION ]] || agent_die "current release link is unsafe"
previous_version=$(tr -d '[:space:]' < "$previous_release/VERSION")
release_dir="$install_root/releases/$version"
agent_install_release_tree "$package_root" "$release_dir" "$version" "$install_root"

systemctl stop wenzwork-device-agent.service
backup_dir=$(agent_create_backup "$data_root" "$env_file" "$backup_root" "$previous_version")
if agent_atomic_symlink "releases/$version" "$install_root/current" &&
  agent_render_template "$release_dir/systemd/wenzwork-device-agent.service" \
    /etc/systemd/system/wenzwork-device-agent.service "$install_root" "$env_file" &&
  systemctl daemon-reload && systemctl start wenzwork-device-agent.service &&
  "$release_dir/scripts/healthcheck.sh" --wait 60; then
  agent_prune_backups "$backup_root" 5
  agent_log "upgrade to $version completed; pre-upgrade backup retained at $backup_dir"
  exit 0
fi

agent_log "upgrade failed; restoring release $previous_version and its complete data snapshot"
systemctl stop wenzwork-device-agent.service 2>/dev/null || true
agent_restore_backup "$backup_dir" "$data_root" "$env_file" wenzwork-agent:wenzwork-agent "$backup_root"
agent_atomic_symlink "$previous_target" "$install_root/current"
agent_render_template "$previous_release/systemd/wenzwork-device-agent.service" \
  /etc/systemd/system/wenzwork-device-agent.service "$install_root" "$env_file"
systemctl daemon-reload
systemctl start wenzwork-device-agent.service
"$previous_release/scripts/healthcheck.sh" --wait 60 ||
  agent_die "upgrade and rollback both failed; leave the service stopped and restore $backup_dir manually"
agent_die "upgrade failed and release $previous_version plus its data were restored"
