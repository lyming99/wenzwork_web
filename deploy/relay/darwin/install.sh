#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

management_url=''
install_root=''
install_root_explicit=false
archive=''
archive_url=''
checksums=''
checksums_url=''
signature=''
signature_url=''
signing_key=''
verifier=''
verifier_explicit=false
verifier_url=''
verifier_sha256=''
access_key_stdin=false
access_key_file=''

while (($#)); do
  case "$1" in
    --management-url|--control-url) management_url=${2:-}; shift 2 ;;
    --install-root|--work-dir) install_root=${2:-}; install_root_explicit=true; shift 2 ;;
    --package-file) archive=${2:-}; shift 2 ;;
    --artifact-url) archive_url=${2:-}; shift 2 ;;
    --checksums-file) checksums=${2:-}; shift 2 ;;
    --checksums-url) checksums_url=${2:-}; shift 2 ;;
    --checksums-signature-file) signature=${2:-}; shift 2 ;;
    --checksums-signature-url) signature_url=${2:-}; shift 2 ;;
    --signing-key-file) signing_key=${2:-}; shift 2 ;;
    --verifier-file) verifier=${2:-}; verifier_explicit=true; shift 2 ;;
    --verifier-url) verifier_url=${2:-}; shift 2 ;;
    --verifier-sha256) verifier_sha256=${2:-}; shift 2 ;;
    --access-key-stdin) access_key_stdin=true; shift ;;
    --access-key-file) access_key_file=${2:-}; shift 2 ;;
    *) relay_die "unknown argument: $1" ;;
  esac
done

relay_require_root
default_install_root=/usr/local/lib/wenzwork-relay
if [[ $install_root_explicit == false ]]; then
  if [[ -t 0 ]]; then
    IFS= read -r -p "Relay install/work directory [$default_install_root]: " install_root
    [[ -n $install_root ]] || install_root=$default_install_root
  else
    install_root=$default_install_root
    relay_log "using non-interactive default install/work directory: $install_root"
  fi
fi
[[ -n $install_root ]] || relay_die "--install-root must not be empty"
install_root=$(relay_validate_install_root "$install_root")
relay_preflight_host "$install_root"
config_root='/Library/Application Support/WenzWork/Relay'
env_file="$config_root/relay.env"
plist='/Library/LaunchDaemons/com.wenzwork.relay.plist'

work_dir=$(mktemp -d /tmp/wenzwork-relay-install.XXXXXX)
cleanup() {
  rm -rf -- "$work_dir"
  case "$script_dir" in
    /tmp/wenzwork-relay-bootstrap.??????) rm -rf -- "$script_dir" ;;
  esac
}
trap cleanup EXIT HUP INT TERM

[[ -n $signing_key ]] || signing_key="$script_dir/../release-signing-public-key.pem"
if [[ -n $verifier_url ]]; then
  [[ -z $verifier ]] || relay_die "select either --verifier-file or --verifier-url"
  [[ $verifier_sha256 =~ ^[0-9a-fA-F]{64}$ ]] || relay_die "--verifier-sha256 is required with --verifier-url"
  verifier="$work_dir/relayctl"
  relay_download "$verifier_url" "$verifier"
  relay_verify_executable_sha256 "$verifier" "$verifier_sha256"
  chmod 0700 "$verifier"
fi
if [[ $verifier_explicit == true ]]; then
  relay_verify_executable_sha256 "$verifier" "$verifier_sha256"
fi
[[ -n $verifier ]] || verifier=/usr/local/bin/relayctl

if [[ -n $archive_url ]]; then
  [[ -z $archive ]] || relay_die "select either --package-file or --artifact-url"
  [[ -n $checksums_url && -n $signature_url ]] || relay_die "URL installation requires checksum URLs"
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
  nested=$(find "$package_root" -mindepth 1 -maxdepth 1 -type d -print -quit)
  [[ -n $nested ]] || relay_die "package root is missing"
  package_root=$nested
fi
[[ -x $package_root/bin/wenzwork-relay-server && -x $package_root/bin/relayctl &&
    -x $package_root/scripts/install.sh && -x $package_root/scripts/upgrade.sh &&
    -x $package_root/scripts/start.sh && -x $package_root/scripts/stop.sh &&
    -x $package_root/scripts/healthcheck.sh && -x $package_root/scripts/uninstall.sh &&
    -f $package_root/scripts/lib/common.sh && -f $package_root/launchd/com.wenzwork.relay.plist &&
    -f $package_root/release-manifest.json && -f $package_root/VERSION ]] || relay_die "macOS Relay package is incomplete"
version=$(tr -d '[:space:]' < "$package_root/VERSION")
[[ $version =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$ ]] || relay_die "package VERSION is invalid"
relay_verify_release_tree "$package_root" "$version" || relay_die "Release manifest verification failed"

mkdir -p "$install_root/releases" "$config_root"
chmod 0755 "$install_root" "$install_root/releases"
chmod 0700 "$config_root"
relay_write_install_metadata "$install_root"
release_dir="$install_root/releases/$version"
relay_install_release_tree "$package_root" "$release_dir" "$version" "$install_root"
relay_atomic_symlink "releases/$version" "$install_root/current"

if [[ -e /usr/local/bin/relayctl && ! -L /usr/local/bin/relayctl ]]; then
  relay_die "/usr/local/bin/relayctl exists and is not a managed symlink"
fi
relay_atomic_symlink "$install_root/current/bin/relayctl" /usr/local/bin/relayctl

if [[ -f $env_file ]]; then
  relay_update_env_version "$env_file" "$version"
  relay_log "existing Access Key and management URL preserved"
else
  if [[ -z $management_url ]]; then
    if [[ -t 0 ]]; then
      IFS= read -r -p 'Management URL [https://wenzwork.com]: ' management_url
      [[ -n $management_url ]] || management_url=https://wenzwork.com
    else
      relay_die "--management-url is required for non-interactive installation"
    fi
  fi
  relay_validate_url "$management_url"
  if [[ $access_key_stdin == true && -n $access_key_file ]]; then relay_die "select one Access Key input mode"; fi
  if [[ $access_key_stdin == false && -z $access_key_file ]]; then
    [[ -t 0 ]] || relay_die "use --access-key-stdin or --access-key-file"
    access_key_stdin=true
  fi
  access_key=$(relay_read_access_key "$access_key_stdin" "$access_key_file")
  relay_write_env "$env_file" "$access_key" "$management_url" "$version"
  access_key=''
  [[ -z $access_key_file ]] || rm -f -- "$access_key_file"
fi

relay_render_launchd_plist "$release_dir/launchd/com.wenzwork.relay.plist" "$plist" "$install_root" "$env_file"
relay_launchd_reload "$plist"
"$release_dir/scripts/healthcheck.sh" --ready --wait 60
relay_log "macOS Relay $version installed under $install_root"
