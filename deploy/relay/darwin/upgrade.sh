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
verifier=''
verifier_explicit=false
verifier_sha256=''
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
    --verifier-file) verifier=${2:-}; verifier_explicit=true; shift 2 ;;
    --verifier-sha256) verifier_sha256=${2:-}; shift 2 ;;
    --install-root|--work-dir) install_root=${2:-}; shift 2 ;;
    --confirm-drained) confirmed=true; shift ;;
    *) relay_die "unknown argument: $1" ;;
  esac
done

relay_require_root
[[ -n $install_root ]] || install_root=$(relay_installed_root)
install_root=$(relay_validate_install_root "$install_root")
relay_preflight_host "$install_root"
[[ -L $install_root/current ]] || relay_die "current release link is missing"
[[ -n $verifier ]] || verifier="$install_root/current/bin/relayctl"
if [[ $verifier_explicit == true ]]; then
  relay_verify_executable_sha256 "$verifier" "$verifier_sha256"
fi
[[ -n $signing_key ]] || signing_key="$script_dir/../release-signing-public-key.pem"
if [[ $confirmed == false ]]; then
  if [[ -t 0 ]]; then
    confirmation=''
    IFS= read -r -p '请先排空节点并移出外部 LB；输入 UPGRADE 继续: ' confirmation
    [[ $confirmation == UPGRADE ]] || relay_die "upgrade cancelled"
  else
    relay_die "pass --confirm-drained after draining the node"
  fi
fi

work_dir=$(mktemp -d /tmp/wenzwork-relay-upgrade.XXXXXX)
# Invoked indirectly by the signal and exit trap below.
# shellcheck disable=SC2317
cleanup() {
  rm -rf -- "$work_dir"
  case "$script_dir" in
    /tmp/wenzwork-relay-bootstrap.??????) rm -rf -- "$script_dir" ;;
  esac
}
trap cleanup EXIT HUP INT TERM
if [[ -n $archive_url ]]; then
  [[ -z $archive ]] || relay_die "select either --package-file or --artifact-url"
  [[ -n $checksums_url && -n $signature_url ]] || relay_die "URL upgrade requires checksum URLs"
  archive_name=$(basename "${archive_url%%\?*}")
  [[ $archive_name == *.tar.gz && $archive_name != */* ]] || relay_die "artifact URL must end with .tar.gz"
  archive="$work_dir/$archive_name"
  checksums="$work_dir/SHA256SUMS"
  signature="$work_dir/SHA256SUMS.sig"
  relay_download "$archive_url" "$archive"
  relay_download "$checksums_url" "$checksums"
  relay_download "$signature_url" "$signature"
fi
[[ -f $archive && -f $checksums && -f $signature && -f $signing_key ]] || relay_die "signed Relay package inputs are required"
relay_verify_bundle "$archive" "$checksums" "$signature" "$signing_key" "$verifier"
relay_extract_bundle "$archive" "$work_dir/package"
package_root="$work_dir/package"
if [[ ! -x $package_root/bin/wenzwork-relay-server ]]; then
  package_root=$(find "$package_root" -mindepth 1 -maxdepth 1 -type d -print -quit)
fi
[[ -n $package_root && -x $package_root/bin/wenzwork-relay-server && -x $package_root/bin/relayctl &&
    -x $package_root/scripts/upgrade.sh && -x $package_root/scripts/healthcheck.sh &&
    -f $package_root/scripts/lib/common.sh && -f $package_root/launchd/com.wenzwork.relay.plist &&
    -f $package_root/release-manifest.json && -f $package_root/VERSION ]] || relay_die "macOS Relay package is incomplete"
version=$(tr -d '[:space:]' < "$package_root/VERSION")
relay_verify_release_tree "$package_root" "$version" || relay_die "Release manifest verification failed"

previous_target=$(readlink "$install_root/current")
previous_release=$(cd "$install_root/current" && pwd -P)
[[ $previous_release == "$install_root"/releases/* ]] || relay_die "current release link is unsafe"
release_dir="$install_root/releases/$version"
relay_install_release_tree "$package_root" "$release_dir" "$version" "$install_root"
config_root='/Library/Application Support/WenzWork/Relay'
env_file="$config_root/relay.env"
plist='/Library/LaunchDaemons/com.wenzwork.relay.plist'
cp -p "$env_file" "$work_dir/relay.env.backup"

launchctl bootout system/com.wenzwork.relay >/dev/null 2>&1 || true
if relay_atomic_symlink "releases/$version" "$install_root/current" &&
  relay_update_env_version "$env_file" "$version" &&
  relay_render_launchd_plist "$release_dir/launchd/com.wenzwork.relay.plist" "$plist" "$install_root" "$env_file" &&
  relay_launchd_reload "$plist" &&
  "$release_dir/scripts/healthcheck.sh" --ready --wait 60; then
  relay_log "upgrade to $version completed"
  exit 0
fi

relay_log "upgrade failed; rolling back to $previous_target"
launchctl bootout system/com.wenzwork.relay >/dev/null 2>&1 || true
relay_atomic_symlink "$previous_target" "$install_root/current"
cp -p "$work_dir/relay.env.backup" "$env_file"
relay_render_launchd_plist "$previous_release/launchd/com.wenzwork.relay.plist" "$plist" "$install_root" "$env_file"
relay_launchd_reload "$plist"
"$previous_release/scripts/healthcheck.sh" --ready --wait 60 || relay_die "rollback failed"
relay_die "upgrade failed and the previous version was restored"
