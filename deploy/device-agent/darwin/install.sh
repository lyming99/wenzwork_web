#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
common="$script_dir/lib/common.sh"
[[ -f $common ]] || common="$script_dir/../lib/common.sh"
# shellcheck source=../lib/common.sh
source "$common"

install_root=/usr/local/lib/wenzwork-device-agent
archive=''
archive_url=''
checksums=''
checksums_url=''
signature=''
signature_url=''
signing_key=''
verifier=''
verifier_sha256=''
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
    --verifier-file) verifier=${2:-}; shift 2 ;;
    --verifier-sha256) verifier_sha256=${2:-}; shift 2 ;;
    --agent-env-file) env_source=${2:-}; shift 2 ;;
    *) agent_die "unknown argument: $1" ;;
  esac
done

agent_require_root
[[ $(agent_host_platform) == darwin ]] || agent_die "this installer requires macOS"
install_root=$(agent_validate_install_root "$install_root")
config_root=$(agent_validate_config_root '/Library/Application Support/WenzWork/DeviceAgent/config')
data_root=$(agent_validate_data_root '/Library/Application Support/WenzWork/DeviceAgent/data')
if [[ -d $data_root ]]; then agent_assert_atomic_restore_layout "$data_root"; fi
plist=/Library/LaunchDaemons/com.wenzwork.device-agent.plist
if [[ -z $signing_key ]]; then
  if [[ -f $script_dir/release-signing-public-key.pem ]]; then
    signing_key="$script_dir/release-signing-public-key.pem"
  else
    signing_key="$script_dir/../release-signing-public-key.pem"
  fi
fi
[[ -f $signing_key && ! -L $signing_key ]] || agent_die "a trusted Release signing public key is required"
[[ -n $verifier ]] || agent_die "initial macOS installation requires --verifier-file from the authenticated bootstrap channel"
agent_verify_executable_sha256 "$verifier" "$verifier_sha256"
[[ -x $verifier && ! -L $verifier ]] || agent_die "bootstrap verifier must already be an executable regular file"
codesign --verify --strict --verbose=2 "$verifier" >/dev/null 2>&1 ||
  agent_die "bootstrap verifier Developer ID signature is invalid"
spctl --assess --type execute --verbose=2 "$verifier" >/dev/null 2>&1 ||
  agent_die "bootstrap verifier is not accepted by Gatekeeper/notarization policy"

work_dir=$(mktemp -d /tmp/wenzwork-device-agent-install.XXXXXX)
# shellcheck disable=SC2329 # Invoked indirectly by trap.
cleanup() { agent_remove_temp_tree "$work_dir"; }
trap cleanup EXIT HUP INT TERM
package_info=$(agent_prepare_package "$archive" "$archive_url" "$checksums" "$checksums_url" \
  "$signature" "$signature_url" "$signing_key" "$verifier" "$work_dir")
package_root=${package_info%%$'\n'*}
version=${package_info#*$'\n'}

if ! dscl . -read /Groups/_wenzworkagent >/dev/null 2>&1; then
  service_id=398
  while dscl . -search /Groups PrimaryGroupID "$service_id" 2>/dev/null | grep -q . ||
    dscl . -search /Users UniqueID "$service_id" 2>/dev/null | grep -q .; do
    ((service_id -= 1))
    (( service_id >= 300 )) || agent_die "could not allocate a protected service account ID"
  done
  dscl . -create /Groups/_wenzworkagent
  dscl . -create /Groups/_wenzworkagent PrimaryGroupID "$service_id"
  dscl . -create /Groups/_wenzworkagent RealName 'WenzWork Device Agent'
fi
service_id=$(dscl . -read /Groups/_wenzworkagent PrimaryGroupID | awk '{print $2}')
[[ $service_id =~ ^[0-9]+$ && $service_id -ge 300 && $service_id -le 398 ]] ||
  agent_die "existing _wenzworkagent group has an unsafe service account ID"
if ! dscl . -read /Users/_wenzworkagent >/dev/null 2>&1; then
  ! dscl . -search /Users UniqueID "$service_id" 2>/dev/null | grep -q . ||
    agent_die "service account ID is already used by another macOS user"
  dscl . -create /Users/_wenzworkagent
  dscl . -create /Users/_wenzworkagent UniqueID "$service_id"
  dscl . -create /Users/_wenzworkagent PrimaryGroupID "$service_id"
  dscl . -create /Users/_wenzworkagent RealName 'WenzWork Device Agent'
  dscl . -create /Users/_wenzworkagent NFSHomeDirectory "$data_root"
  dscl . -create /Users/_wenzworkagent UserShell /usr/bin/false
  dscl . -create /Users/_wenzworkagent IsHidden 1
else
  existing_uid=$(dscl . -read /Users/_wenzworkagent UniqueID | awk '{print $2}')
  existing_gid=$(dscl . -read /Users/_wenzworkagent PrimaryGroupID | awk '{print $2}')
  existing_shell=$(dscl . -read /Users/_wenzworkagent UserShell | awk '{print $2}')
  existing_home=$(dscl . -read /Users/_wenzworkagent NFSHomeDirectory | sed -E 's/^[^:]+:[[:space:]]*//')
  [[ $existing_uid == "$service_id" && $existing_gid == "$service_id" &&
      $existing_shell == /usr/bin/false && $existing_home == "$data_root" ]] ||
    agent_die "existing _wenzworkagent account does not match the managed service identity"
fi

install -d -o root -g _wenzworkagent -m 0750 "$install_root" "$install_root/releases" "$config_root"
install -d -o _wenzworkagent -g _wenzworkagent -m 0700 "$data_root" "$data_root/state" "$data_root/workspace" "$data_root/logs"
agent_assert_atomic_restore_layout "$data_root"
agent_write_install_metadata "$install_root" "$config_root/install.conf" _wenzworkagent
env_file="$config_root/agent.env"
expected_state="$data_root/state/agent-state.json"
if [[ -f $env_file ]]; then
  agent_validate_env_file "$env_file" "$expected_state"
  agent_log "existing Device Access Key and Control Plane configuration preserved"
else
  [[ -n $env_source ]] || agent_die "first installation requires --agent-env-file"
  agent_write_env_file "$env_source" "$env_file" _wenzworkagent:_wenzworkagent "$expected_state"
fi

release_dir="$install_root/releases/$version"
agent_install_release_tree "$package_root" "$release_dir" "$version" "$install_root"
previous_target=''
[[ -L $install_root/current ]] && previous_target=$(readlink "$install_root/current")
agent_atomic_symlink "releases/$version" "$install_root/current"
agent_render_template "$release_dir/launchd/com.wenzwork.device-agent.plist" "$plist" "$install_root" "$env_file"

launchctl bootout system/com.wenzwork.device-agent >/dev/null 2>&1 || true
if launchctl bootstrap system "$plist" &&
  launchctl enable system/com.wenzwork.device-agent &&
  launchctl kickstart -k system/com.wenzwork.device-agent &&
  "$release_dir/scripts/healthcheck.sh" --wait 60; then
  agent_log "macOS Device Agent $version installed under $install_root"
  exit 0
fi

launchctl bootout system/com.wenzwork.device-agent >/dev/null 2>&1 || true
if [[ -n $previous_target ]]; then
  agent_atomic_symlink "$previous_target" "$install_root/current"
  previous_release=$(cd "$install_root/current" && pwd -P)
  agent_render_template "$previous_release/launchd/com.wenzwork.device-agent.plist" "$plist" "$install_root" "$env_file"
  launchctl bootstrap system "$plist"
  launchctl kickstart -k system/com.wenzwork.device-agent
fi
agent_die "installation failed; configuration and generated business data were preserved"
