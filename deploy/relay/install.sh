#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

management_url=''
installation_id=''
install_root=''
install_root_explicit=false
archive=''
archive_url=''
checksums=''
checksums_url=''
checksums_signature=''
checksums_signature_url=''
signing_key=''
access_key_stdin=false
access_key_file=''
token_stdin=false
token_file=''
relay_env_file=''

while (($#)); do
  case "$1" in
    --management-url|--control-url) management_url=${2:-}; shift 2 ;;
    --installation-id) installation_id=${2:-}; shift 2 ;;
    --install-root|--work-dir) install_root=${2:-}; install_root_explicit=true; shift 2 ;;
    --package-file) archive=${2:-}; shift 2 ;;
    --artifact-url) archive_url=${2:-}; shift 2 ;;
    --checksums-file) checksums=${2:-}; shift 2 ;;
    --checksums-url) checksums_url=${2:-}; shift 2 ;;
    --checksums-signature-file) checksums_signature=${2:-}; shift 2 ;;
    --checksums-signature-url) checksums_signature_url=${2:-}; shift 2 ;;
    --signing-key-file) signing_key=${2:-}; shift 2 ;;
    --access-key-stdin) access_key_stdin=true; shift ;;
    --access-key-file) access_key_file=${2:-}; shift 2 ;;
    --enrollment-token-stdin) token_stdin=true; shift ;;
    --enrollment-token-file) token_file=${2:-}; shift 2 ;;
    --relay-env-file) relay_env_file=${2:-}; shift 2 ;;
    *) relay_die "unknown argument: $1" ;;
  esac
done

if [[ -z $signing_key ]]; then
  if [[ -f $script_dir/release-signing-public-key.pem ]]; then
    signing_key="$script_dir/release-signing-public-key.pem"
  else
    signing_key="$script_dir/../release-signing-public-key.pem"
  fi
fi

if [[ $install_root_explicit == true && -n $install_root ]]; then
  install_root=$(relay_validate_install_root "$install_root")
fi
if [[ -f /etc/wenzwork-relay/install.conf ]]; then
  installed_root=$(relay_installed_root)
  if [[ $install_root_explicit == true && -n $install_root && $install_root != "$installed_root" ]]; then
    relay_die "Relay is already managed under $installed_root; uninstall before changing the work directory"
  fi
  [[ -n $install_root ]] || install_root=$installed_root
elif [[ -z $install_root && -t 0 ]]; then
  IFS= read -r -p 'Relay 工作/安装目录 [/opt/wenzwork-relay]: ' install_root
fi
[[ -n $install_root ]] || install_root=/opt/wenzwork-relay
install_root=$(relay_validate_install_root "$install_root")

relay_require_root
relay_preflight_host "$install_root"

work_dir=$(mktemp -d /tmp/wenzwork-relay-install.XXXXXX)
access_key=''
token=''
cleanup() {
  access_key=''
  token=''
  relay_remove_temp_tree "$work_dir"
  case "$script_dir" in
    /tmp/wenzwork-relay-bootstrap.??????) relay_remove_temp_tree "$script_dir" ;;
  esac
}
trap cleanup EXIT HUP INT TERM

if [[ -n $archive_url ]]; then
  [[ -z $archive ]] || relay_die "select either --package-file or --artifact-url"
  [[ -n $checksums_url && -n $checksums_signature_url ]] ||
    relay_die "URL installation requires --checksums-url and --checksums-signature-url"
  archive_name=$(basename "${archive_url%%\?*}")
  [[ $archive_name != . && $archive_name != / && $archive_name == *.tar.gz ]] ||
    relay_die "artifact URL must end with a safe .tar.gz file name"
  archive="$work_dir/$archive_name"
  checksums="$work_dir/SHA256SUMS"
  checksums_signature="$work_dir/SHA256SUMS.sig"
  relay_download "$archive_url" "$archive"
  relay_download "$checksums_url" "$checksums"
  relay_download "$checksums_signature_url" "$checksums_signature"
fi
[[ -n $archive ]] || relay_die "a signed Relay package is required"
relay_verify_bundle "$archive" "$checksums" "$checksums_signature" "$signing_key"
relay_extract_bundle "$archive" "$work_dir/package"

package_root="$work_dir/package"
if [[ ! -x $package_root/bin/wenzwork-relay-server ]]; then
  nested=$(find "$package_root" -mindepth 1 -maxdepth 1 -type d -print -quit)
  [[ -n $nested && -x $nested/bin/wenzwork-relay-server ]] || relay_die "package does not contain wenzwork-relay-server"
  package_root=$nested
fi
[[ -x $package_root/bin/relayctl && -x $package_root/bin/wenzwork-relay-server &&
    -x $package_root/scripts/install.sh && -x $package_root/scripts/healthcheck.sh &&
    -x $package_root/scripts/upgrade.sh && -x $package_root/scripts/uninstall.sh &&
    -f $package_root/scripts/lib/common.sh && -f $package_root/VERSION &&
    -f $package_root/release-manifest.json && -f $package_root/config.example.yaml &&
    -f $package_root/relay.env.example && -f $package_root/systemd/wenzwork-relay.service &&
    -f $package_root/release-signing-public-key.pem ]] || relay_die "package is incomplete"
relay_verify_same_public_key "$signing_key" "$package_root/release-signing-public-key.pem"
version=$(tr -d '[:space:]' < "$package_root/VERSION")
[[ $version =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$ ]] || relay_die "package VERSION is invalid"
relay_verify_release_tree "$package_root" "$version" || relay_die "Relay release manifest verification failed"

if ! getent group wenzwork-relay >/dev/null; then groupadd --system wenzwork-relay; fi
if ! id wenzwork-relay >/dev/null 2>&1; then
  useradd --system --gid wenzwork-relay --home-dir /var/lib/wenzwork-relay --shell /usr/sbin/nologin wenzwork-relay
fi
install -d -o root -g wenzwork-relay -m 0750 "$install_root" "$install_root/releases" /etc/wenzwork-relay /etc/wenzwork-relay/tls
install -d -o wenzwork-relay -g wenzwork-relay -m 0700 /var/lib/wenzwork-relay/identity /var/lib/wenzwork-relay/state
relay_write_install_metadata "$install_root"

release_dir="$install_root/releases/$version"
relay_install_release_tree "$package_root" "$release_dir" "$version" "$install_root"

previous_target=''
[[ -L $install_root/current ]] && previous_target=$(readlink "$install_root/current")
relay_atomic_symlink "releases/$version" "$install_root/current"
if [[ -e /usr/local/bin/relayctl && ! -L /usr/local/bin/relayctl ]]; then
  relay_die "/usr/local/bin/relayctl exists and is not a managed symlink"
fi
relay_atomic_symlink "$install_root/current/bin/relayctl" /usr/local/bin/relayctl
relay_render_systemd_unit "$release_dir/systemd/wenzwork-relay.service" /etc/systemd/system/wenzwork-relay.service "$install_root"

existing_env=/etc/wenzwork-relay/relay.env
if [[ -n $relay_env_file ]]; then
  relay_validate_env_file "$relay_env_file"
  install -o wenzwork-relay -g wenzwork-relay -m 0600 "$relay_env_file" "$existing_env"
  relay_update_env_version "$existing_env" "$version"
  relay_log "Relay environment installed non-interactively"
elif [[ -f $existing_env ]]; then
  relay_validate_env_file "$existing_env"
  relay_update_env_version "$existing_env" "$version"
  relay_log "existing Access Key and management address preserved"
elif [[ $token_stdin == true || -n $token_file ]]; then
  relay_validate_url "$management_url"
  [[ $installation_id =~ ^[0-9a-fA-F-]{36}$ ]] || relay_die "--installation-id must be a UUID"
  [[ $token_stdin == true && -z $token_file || $token_stdin == false && -n $token_file ]] ||
    relay_die "select exactly one Enrollment Token input mode"
  token=$(relay_read_token "$token_stdin" "$token_file")
  if ! printf '%s\n' "$token" | "$install_root/current/bin/relayctl" enroll \
    --control-url "$management_url" --installation-id "$installation_id" --token-stdin \
    --release-version "$version" --config-file /etc/wenzwork-relay/config.yaml; then
    [[ -n $previous_target ]] && relay_atomic_symlink "$previous_target" "$install_root/current"
    relay_die "Relay enrollment failed"
  fi
  token=''
  [[ -z $token_file ]] || rm -f -- "$token_file"
else
  if [[ $access_key_stdin == true && -n $access_key_file ]]; then
    relay_die "select only one Access Key input mode"
  fi
  if [[ -z $management_url ]]; then
    if [[ -t 0 ]]; then
      IFS= read -r -p '管理端地址 [https://wenzwork.com]: ' management_url
      [[ -n $management_url ]] || management_url=https://wenzwork.com
    else
      relay_die "--management-url is required for a non-interactive Access Key installation"
    fi
  fi
  relay_validate_url "$management_url"
  if [[ $access_key_stdin == false && -z $access_key_file ]]; then
    [[ -t 0 ]] || relay_die "use --access-key-stdin, --access-key-file, or --relay-env-file"
    access_key_stdin=true
  fi
  access_key=$(relay_read_access_key "$access_key_stdin" "$access_key_file")
  relay_write_access_env "$existing_env" "$access_key" "$management_url" "$version"
  access_key=''
  [[ -z $access_key_file ]] || rm -f -- "$access_key_file"
  relay_log "Access Key authentication configured"
fi

chown -R wenzwork-relay:wenzwork-relay /var/lib/wenzwork-relay
[[ ! -f /var/lib/wenzwork-relay/identity/identity.key ]] || chmod 0600 /var/lib/wenzwork-relay/identity/identity.key
if [[ -f /etc/wenzwork-relay/config.yaml && -f /etc/wenzwork-relay/tls/node.crt ]]; then
  chown root:wenzwork-relay /etc/wenzwork-relay/config.yaml /etc/wenzwork-relay/tls/node.crt
  chmod 0640 /etc/wenzwork-relay/config.yaml /etc/wenzwork-relay/tls/node.crt
fi
systemctl daemon-reload
systemctl enable --now wenzwork-relay.service
"$install_root/current/scripts/healthcheck.sh" --live --wait 45

relay_log "installation completed in $install_root; Relay will connect to the management service and report heartbeats automatically"
