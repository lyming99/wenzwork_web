#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

install_root=/opt/wenzwork-device-agent
data_root=/var/lib/wenzwork-device-agent
config_root=/etc/wenzwork-device-agent
archive=''
archive_url=''
checksums=''
checksums_url=''
signature=''
signature_url=''
signing_key=''
env_source=''

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
    --agent-env-file) env_source=${2:-}; shift 2 ;;
    *) agent_die "unknown argument: $1" ;;
  esac
done

agent_require_root
[[ $(agent_host_platform) == linux ]] || agent_die "this installer requires Linux"
[[ -d /run/systemd/system ]] || agent_die "systemd is required"
install_root=$(agent_validate_install_root "$install_root")
data_root=$(agent_validate_data_root "$data_root")
config_root=$(agent_validate_config_root "$config_root")
if [[ -d $data_root ]]; then agent_assert_atomic_restore_layout "$data_root"; fi
if [[ -z $signing_key ]]; then
  if [[ -f $script_dir/release-signing-public-key.pem ]]; then
    signing_key="$script_dir/release-signing-public-key.pem"
  else
    signing_key="$script_dir/../release-signing-public-key.pem"
  fi
fi
[[ -f $signing_key && ! -L $signing_key ]] || agent_die "a trusted Release signing public key is required"

work_dir=$(mktemp -d /tmp/wenzwork-device-agent-install.XXXXXX)
# shellcheck disable=SC2329 # Invoked indirectly by trap.
cleanup() { agent_remove_temp_tree "$work_dir"; }
trap cleanup EXIT HUP INT TERM

package_info=$(agent_prepare_package "$archive" "$archive_url" "$checksums" "$checksums_url" \
  "$signature" "$signature_url" "$signing_key" '' "$work_dir")
package_root=${package_info%%$'\n'*}
version=${package_info#*$'\n'}

if ! getent group wenzwork-agent >/dev/null; then groupadd --system wenzwork-agent; fi
if ! id wenzwork-agent >/dev/null 2>&1; then
  useradd --system --gid wenzwork-agent --home-dir "$data_root" --shell /usr/sbin/nologin wenzwork-agent
else
  passwd_record=$(getent passwd wenzwork-agent)
  group_record=$(getent group wenzwork-agent)
  account_fields=()
  group_fields=()
  IFS=: read -r -a account_fields <<< "$passwd_record"
  IFS=: read -r -a group_fields <<< "$group_record"
  [[ ${#account_fields[@]} -ge 7 && ${#group_fields[@]} -ge 3 &&
      ${account_fields[0]} == wenzwork-agent && ${account_fields[3]} == "${group_fields[2]}" &&
      ${account_fields[5]} == "$data_root" ]] ||
    agent_die "existing wenzwork-agent account does not match the managed service identity"
  case "${account_fields[6]}" in
    /usr/sbin/nologin|/sbin/nologin|/bin/false) ;;
    *) agent_die "existing wenzwork-agent account must use a non-login shell" ;;
  esac
fi
install -d -o root -g wenzwork-agent -m 0750 "$install_root" "$install_root/releases" "$config_root"
install -d -o wenzwork-agent -g wenzwork-agent -m 0700 "$data_root" "$data_root/state" "$data_root/workspace"
agent_assert_atomic_restore_layout "$data_root"
agent_write_install_metadata "$install_root" "$config_root/install.conf" wenzwork-agent

env_file="$config_root/agent.env"
expected_state="$data_root/state/agent-state.json"
if [[ -f $env_file ]]; then
  agent_validate_env_file "$env_file" "$expected_state"
  agent_log "existing Device Access Key and Control Plane configuration preserved"
else
  [[ -n $env_source ]] || agent_die "first installation requires --agent-env-file"
  agent_write_env_file "$env_source" "$env_file" wenzwork-agent:wenzwork-agent "$expected_state"
fi

release_dir="$install_root/releases/$version"
agent_install_release_tree "$package_root" "$release_dir" "$version" "$install_root"
previous_target=''
[[ -L $install_root/current ]] && previous_target=$(readlink "$install_root/current")
agent_atomic_symlink "releases/$version" "$install_root/current"
agent_render_template "$release_dir/systemd/wenzwork-device-agent.service" \
  /etc/systemd/system/wenzwork-device-agent.service "$install_root" "$env_file"

systemctl daemon-reload
if systemctl enable --now wenzwork-device-agent.service &&
  "$release_dir/scripts/healthcheck.sh" --wait 45; then
  agent_log "Device Agent $version installed under $install_root; data is under $data_root"
  exit 0
fi

agent_log "installation health check failed"
systemctl disable --now wenzwork-device-agent.service 2>/dev/null || true
if [[ -n $previous_target ]]; then
  agent_atomic_symlink "$previous_target" "$install_root/current"
  previous_release=$(cd "$install_root/current" && pwd -P)
  agent_render_template "$previous_release/systemd/wenzwork-device-agent.service" \
    /etc/systemd/system/wenzwork-device-agent.service "$install_root" "$env_file"
  systemctl daemon-reload
  systemctl enable --now wenzwork-device-agent.service
fi
agent_die "installation failed; configuration and generated business data were preserved"
