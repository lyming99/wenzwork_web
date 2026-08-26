#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

archive=''
archive_url=''
checksums=''
checksums_url=''
signature=''
signature_url=''
signing_key=''
install_root=''
confirmed=false
while (($#)); do
  case "$1" in
    --package-file) archive=${2:-}; shift 2 ;;
    --artifact-url) archive_url=${2:-}; shift 2 ;;
    --checksums-file) checksums=${2:-}; shift 2 ;;
    --checksums-url) checksums_url=${2:-}; shift 2 ;;
    --checksums-signature-file) signature=${2:-}; shift 2 ;;
    --checksums-signature-url) signature_url=${2:-}; shift 2 ;;
    --signing-key-file) signing_key=${2:-}; shift 2 ;;
    --install-root|--work-dir) install_root=${2:-}; shift 2 ;;
    --confirm-drained) confirmed=true; shift ;;
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
[[ -n $install_root ]] || install_root=$(relay_installed_root)
install_root=$(relay_validate_install_root "$install_root")

relay_require_root
relay_preflight_host "$install_root"
if [[ $confirmed == false ]]; then
  if [[ -t 0 ]]; then
    confirmation=''
    IFS= read -r -p '请先排空节点并移出外部 LB；输入 UPGRADE 继续: ' confirmation
    [[ $confirmation == UPGRADE ]] || relay_die "upgrade cancelled"
  else
    relay_die "drain the node, remove it from the external LB, then pass --confirm-drained"
  fi
fi
[[ -L $install_root/current ]] || relay_die "Relay current release link is missing"

work_dir=$(mktemp -d /tmp/wenzwork-relay-upgrade.XXXXXX)
# Invoked indirectly by the signal and exit trap below.
# shellcheck disable=SC2317
cleanup() {
  relay_remove_temp_tree "$work_dir"
  case "$script_dir" in
    /tmp/wenzwork-relay-bootstrap.??????) relay_remove_temp_tree "$script_dir" ;;
  esac
}
trap cleanup EXIT HUP INT TERM

if [[ -n $archive_url ]]; then
  [[ -z $archive ]] || relay_die "select either --package-file or --artifact-url"
  [[ -n $checksums_url && -n $signature_url ]] ||
    relay_die "URL upgrade requires --checksums-url and --checksums-signature-url"
  archive_name=$(basename "${archive_url%%\?*}")
  [[ $archive_name != . && $archive_name != / && $archive_name == *.tar.gz ]] ||
    relay_die "artifact URL must end with a safe .tar.gz file name"
  archive="$work_dir/$archive_name"
  checksums="$work_dir/SHA256SUMS"
  signature="$work_dir/SHA256SUMS.sig"
  relay_download "$archive_url" "$archive"
  relay_download "$checksums_url" "$checksums"
  relay_download "$signature_url" "$signature"
fi
[[ -n $archive ]] || relay_die "a signed Relay package is required"
relay_verify_bundle "$archive" "$checksums" "$signature" "$signing_key"
relay_extract_bundle "$archive" "$work_dir/package"

package_root="$work_dir/package"
if [[ ! -x $package_root/bin/wenzwork-relay-server ]]; then
  package_root=$(find "$package_root" -mindepth 1 -maxdepth 1 -type d -print -quit)
fi
[[ -n $package_root && -x $package_root/bin/wenzwork-relay-server && -x $package_root/bin/relayctl &&
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

previous_target=$(readlink "$install_root/current")
previous_release=$(readlink -f "$install_root/current")
[[ $previous_release == "$install_root"/releases/* && -d $previous_release ]] ||
  relay_die "current Relay release link is unsafe"
release_dir="$install_root/releases/$version"
relay_install_release_tree "$package_root" "$release_dir" "$version" "$install_root"

env_file=/etc/wenzwork-relay/relay.env
if [[ -f $env_file ]]; then
  relay_validate_env_file "$env_file"
  cp -a -- "$env_file" "$work_dir/relay.env.backup"
fi

update_release_environment() {
  [[ ! -f $env_file ]] || relay_update_env_version "$env_file" "$version"
}

if systemctl stop wenzwork-relay.service &&
  relay_atomic_symlink "releases/$version" "$install_root/current" &&
  relay_render_systemd_unit "$release_dir/systemd/wenzwork-relay.service" /etc/systemd/system/wenzwork-relay.service "$install_root" &&
  update_release_environment &&
  systemctl daemon-reload &&
  systemctl start wenzwork-relay.service &&
  "$release_dir/scripts/healthcheck.sh" --ready --wait 60; then
  relay_log "upgrade to $version completed; existing configuration and state were preserved"
  exit 0
fi

relay_log "upgrade health check failed; rolling back to $previous_target"
systemctl stop wenzwork-relay.service || true
relay_atomic_symlink "$previous_target" "$install_root/current"
if [[ -f $previous_release/systemd/wenzwork-relay.service ]]; then
  relay_render_systemd_unit "$previous_release/systemd/wenzwork-relay.service" /etc/systemd/system/wenzwork-relay.service "$install_root"
fi
[[ ! -f $work_dir/relay.env.backup ]] || install -o wenzwork-relay -g wenzwork-relay -m 0600 "$work_dir/relay.env.backup" "$env_file"
systemctl daemon-reload
systemctl start wenzwork-relay.service
"$install_root/current/scripts/healthcheck.sh" --ready --wait 60 ||
  relay_die "rollback also failed; keep the host out of the LB and investigate"
relay_die "upgrade failed and the previous version was restored"
