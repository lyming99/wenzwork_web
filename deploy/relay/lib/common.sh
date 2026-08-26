#!/usr/bin/env bash
set -euo pipefail

relay_log() { printf '[wenzwork-relay] %s\n' "$*" >&2; }
relay_die() { relay_log "ERROR: $*"; exit 1; }

relay_require_root() {
  [[ ${EUID:-$(id -u)} -eq 0 ]] || relay_die "run this script with sudo or as root"
}

relay_require_command() {
  command -v "$1" >/dev/null 2>&1 || relay_die "required command is missing: $1"
}

relay_validate_url() {
  local value=$1
  [[ $value =~ ^https?://[^[:space:]]+$ ]] ||
    relay_die "network URLs must use HTTP or HTTPS"
}

relay_validate_install_root() {
  local value=${1:-}
  [[ $value == / ]] || value=${value%/}
  [[ $value =~ ^/[A-Za-z0-9._/-]+$ && $value != *//* && $value != */./* && $value != */../* && $value != */. && $value != */.. ]] ||
    relay_die "Relay work directory must be a normalized absolute path without whitespace"
  case "$value" in
    /|/bin|/boot|/dev|/etc|/home|/lib|/lib64|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/var)
      relay_die "Relay work directory is too broad: $value"
      ;;
  esac
  [[ ! -L $value ]] || relay_die "Relay work directory must not be a symbolic link"
  printf '%s' "$value"
}

relay_installed_root() {
  local metadata=${RELAY_INSTALL_METADATA:-/etc/wenzwork-relay/install.conf} value=''
  if [[ -f $metadata && ! -L $metadata ]]; then
    value=$(awk -F= '$1 == "RELAY_INSTALL_ROOT" {print substr($0, index($0, "=") + 1); exit}' "$metadata")
  fi
  [[ -n $value ]] || value=/opt/wenzwork-relay
  relay_validate_install_root "$value"
}

relay_write_install_metadata() {
  local install_root=$1 metadata=${RELAY_INSTALL_METADATA:-/etc/wenzwork-relay/install.conf}
  install_root=$(relay_validate_install_root "$install_root")
  local temporary="${metadata}.new.$$"
  printf 'RELAY_INSTALL_ROOT=%s\n' "$install_root" > "$temporary"
  chown root:wenzwork-relay "$temporary"
  chmod 0640 "$temporary"
  mv -f -- "$temporary" "$metadata"
}

relay_render_systemd_unit() {
  local source=$1 destination=$2 install_root=$3
  local temporary="${destination}.new.$$"
  install_root=$(relay_validate_install_root "$install_root")
  sed "s|/opt/wenzwork-relay|$install_root|g" "$source" > "$temporary"
  chown root:root "$temporary"
  chmod 0644 "$temporary"
  mv -f -- "$temporary" "$destination"
}

relay_normalize_architecture() {
  case "${1:-}" in
    x86_64|amd64) printf 'amd64' ;;
    aarch64|arm64) printf 'arm64' ;;
    *) relay_die "supported Relay architectures are amd64/x86_64 and arm64/aarch64" ;;
  esac
}

relay_host_platform() {
  [[ $(uname -s) == Linux ]] || relay_die "only Linux Relay hosts are supported"
  printf 'linux'
}

relay_host_architecture() {
  relay_normalize_architecture "$(uname -m)"
}

relay_preflight_host() {
  local install_root=${1:-/opt/wenzwork-relay} disk_path
  install_root=$(relay_validate_install_root "$install_root")
  relay_host_platform >/dev/null
  relay_host_architecture >/dev/null
  [[ -d /run/systemd/system ]] || relay_die "systemd is required"
  # shellcheck disable=SC1091
  source /etc/os-release
  case "${ID:-}:${VERSION_ID:-}" in
    ubuntu:22.04|ubuntu:24.04|debian:12) ;;
    *) relay_die "supported systems are Ubuntu 22.04/24.04 and Debian 12" ;;
  esac
  disk_path=$install_root
  while [[ ! -e $disk_path && $disk_path != / ]]; do disk_path=$(dirname "$disk_path"); done
  local free_kib
  free_kib=$(df -Pk "$disk_path" | awk 'NR==2 {print $4}')
  [[ $free_kib =~ ^[0-9]+$ && $free_kib -ge 524288 ]] || relay_die "at least 512 MiB free space is required for the Relay work directory"
  if command -v timedatectl >/dev/null 2>&1; then
    local synchronized
    synchronized=$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)
    [[ $synchronized != no ]] || relay_die "system clock is not synchronized"
  fi
}

relay_verify_bundle() {
  local archive=$1 checksums=$2 signature=$3 public_key=$4
  [[ -f $archive && -f $checksums && -f $signature && -f $public_key ]] ||
    relay_die "archive, SHA256SUMS, signature, and signing public key are required"
  relay_require_command openssl
  relay_require_command sha256sum
  openssl pkeyutl -verify -pubin -inkey "$public_key" -rawin -in "$checksums" -sigfile "$signature" >/dev/null 2>&1 ||
    relay_die "SHA256SUMS signature verification failed"
  local archive_dir archive_name expected_line
  archive_dir=$(dirname "$archive")
  archive_name=$(basename "$archive")
  expected_line=$(awk -v name="$archive_name" '$2 == name || $2 == "*" name {print; found=1} END {if (!found) exit 1}' "$checksums") ||
    relay_die "SHA256SUMS does not contain the selected archive"
  (cd "$archive_dir" && printf '%s\n' "$expected_line" | sha256sum -c - >/dev/null) ||
    relay_die "Relay archive SHA-256 verification failed"
}

relay_verify_same_public_key() {
  local trusted_key=$1 packaged_key=$2 trusted_digest packaged_digest
  [[ -f $trusted_key && -f $packaged_key ]] || relay_die "trusted and packaged Release public keys are required"
  trusted_digest=$(openssl pkey -pubin -in "$trusted_key" -outform DER 2>/dev/null | sha256sum | awk '{print $1}') ||
    relay_die "trusted Release public key is invalid"
  packaged_digest=$(openssl pkey -pubin -in "$packaged_key" -outform DER 2>/dev/null | sha256sum | awk '{print $1}') ||
    relay_die "packaged Release public key is invalid"
  [[ -n $trusted_digest && $trusted_digest == "$packaged_digest" ]] ||
    relay_die "packaged Release public key does not match the trusted key"
}

relay_extract_bundle() {
  local archive=$1 destination=$2
  relay_require_command tar
  local listing entry_type
  while IFS= read -r listing; do
    entry_type=${listing:0:1}
    [[ $entry_type == '-' || $entry_type == d ]] ||
      relay_die "archive contains an unsupported link or special file"
  done < <(LC_ALL=C tar -tvzf "$archive")
  while IFS= read -r entry; do
    [[ $entry != /* && $entry != ../* && $entry != */../* && $entry != *'/..' ]] ||
      relay_die "archive contains an unsafe path"
  done < <(tar -tzf "$archive")
  mkdir -p "$destination"
  tar -xzf "$archive" -C "$destination" --no-same-owner --no-same-permissions
}

relay_download() {
  local url=$1 destination=$2
  relay_validate_url "$url"
  relay_require_command curl
  if [[ $url == https://* ]]; then
    curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
      --connect-timeout 10 --max-time 600 --output "$destination" "$url"
  else
    curl --fail --silent --show-error --location --proto '=http' --proto-redir '=http' \
      --connect-timeout 10 --max-time 600 --output "$destination" "$url"
  fi
}

relay_remove_temp_tree() {
  local target=${1:-}
  case "$target" in
    /tmp/wenzwork-relay-install.??????|/tmp/wenzwork-relay-upgrade.??????|/tmp/wenzwork-relay-diagnostics.??????|/tmp/wenzwork-relay-bootstrap.??????)
      rm -rf -- "$target"
      ;;
    *)
      relay_log "refusing to remove unexpected temporary path: $target"
      return 1
      ;;
  esac
}

relay_remove_release_stage() {
  local target=${1:-} releases_root=${2:-}
  [[ -n $target && -n $releases_root && $target == "$releases_root"/.release.?????? && $releases_root == /*/releases ]] || {
    relay_log "refusing to remove unexpected release stage: $target"
    return 1
  }
  rm -rf -- "$target"
}

relay_verify_release_tree() {
  local release_root=$1 expected_version=$2 expected_platform expected_architecture
  [[ -x $release_root/bin/relayctl ]] || return 1
  expected_platform=$(relay_host_platform)
  expected_architecture=$(relay_host_architecture)
  "$release_root/bin/relayctl" release verify --root "$release_root" \
    --manifest release-manifest.json --expected-version "$expected_version" \
    --expected-platform "$expected_platform" --expected-architecture "$expected_architecture" --protocol-version 1 >/dev/null
}

relay_install_release_tree() {
  local package_root=$1 release_dir=$2 expected_version=$3 install_root=${4:-/opt/wenzwork-relay} stage='' releases_root
  install_root=$(relay_validate_install_root "$install_root")
  releases_root="$install_root/releases"
  [[ $release_dir == "$releases_root/$expected_version" ]] ||
    relay_die "Relay release destination is invalid"
  if [[ -L $release_dir ]]; then
    relay_die "Relay release destination must not be a symbolic link"
  fi
  if [[ -d $release_dir ]]; then
    relay_verify_release_tree "$release_dir" "$expected_version" ||
      relay_die "existing Relay release manifest verification failed"
    relay_log "existing verified release $expected_version preserved"
    return 0
  fi
  [[ ! -e $release_dir ]] || relay_die "Relay release destination is not a directory"

  stage=$(mktemp -d "$releases_root/.release.XXXXXX")
  if ! {
    cp -a -- "$package_root/." "$stage/" &&
      chown -R root:root "$stage" &&
      find "$stage" -type d -exec chmod 0755 {} + &&
      find "$stage" -type f -exec chmod 0644 {} + &&
      chmod 0755 "$stage/bin/wenzwork-relay-server" "$stage/bin/relayctl" "$stage/scripts/"*.sh &&
      relay_verify_release_tree "$stage" "$expected_version"
  }; then
    relay_remove_release_stage "$stage" "$releases_root"
    relay_die "could not stage the verified Relay release"
  fi
  if ! mv -T -- "$stage" "$release_dir"; then
    relay_remove_release_stage "$stage" "$releases_root"
    relay_die "could not install the Relay release directory"
  fi
}

relay_atomic_symlink() {
  local target=$1 link=$2
  local temporary="${link}.new.$$"
  ln -s "$target" "$temporary"
  mv -Tf "$temporary" "$link"
}

relay_read_token() {
  local from_stdin=$1 token_file=$2 token=''
  if [[ $from_stdin == true ]]; then
    if [[ -t 0 ]]; then
      IFS= read -r -s -p 'Enrollment Token: ' token
      printf '\n' >&2
    else
      IFS= read -r token
    fi
  else
    [[ -f $token_file ]] || relay_die "Enrollment Token file does not exist"
    local permissions
    permissions=$(stat -c '%a' "$token_file")
    [[ $permissions == 600 ]] || relay_die "Enrollment Token file permissions must be 0600"
    IFS= read -r token < "$token_file"
  fi
  [[ $token =~ ^[A-Za-z0-9_-]{43,128}$ ]] || relay_die "Enrollment Token is invalid"
  printf '%s' "$token"
}

relay_read_access_key() {
  local from_stdin=$1 key_file=$2 key=''
  if [[ $from_stdin == true ]]; then
    if [[ -t 0 ]]; then
      IFS= read -r -s -p 'Access Key: ' key
      printf '\n' >&2
    else
      IFS= read -r key
    fi
  else
    [[ -f $key_file && ! -L $key_file ]] || relay_die "Access Key file does not exist or is a symbolic link"
    local permissions
    permissions=$(stat -c '%a' "$key_file")
    [[ $permissions == 600 ]] || relay_die "Access Key file permissions must be 0600"
    IFS= read -r key < "$key_file"
  fi
  [[ $key =~ ^relay_[A-Za-z0-9_-]{43}$ ]] || relay_die "Access Key is invalid"
  printf '%s' "$key"
}

relay_env_value() {
  local file=$1 name=$2
  awk -F= -v name="$name" '$1 == name {print substr($0, index($0, "=") + 1); exit}' "$file"
}

relay_validate_env_file() {
  local file=$1 access_key management_url permissions
  [[ -f $file && ! -L $file ]] || relay_die "Relay environment must be a regular file"
  permissions=$(stat -c '%a' "$file")
  [[ $permissions == 600 || $permissions == 640 ]] || relay_die "Relay environment permissions must be 0600 or 0640"
  access_key=$(relay_env_value "$file" RELAY_ACCESS_KEY)
  [[ $access_key =~ ^relay_[A-Za-z0-9_-]{43}$ ]] || relay_die "Relay environment is missing a valid RELAY_ACCESS_KEY"
  management_url=$(relay_env_value "$file" RELAY_MANAGEMENT_URL)
  [[ -z $management_url ]] || relay_validate_url "$management_url"
}

relay_write_access_env() {
  local destination=$1 access_key=$2 management_url=$3 version=$4
  local temporary="${destination}.new.$$"
  [[ $access_key =~ ^relay_[A-Za-z0-9_-]{43}$ ]] || relay_die "Access Key is invalid"
  relay_validate_url "$management_url"
  [[ $version =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$ ]] || relay_die "Relay version is invalid"
  printf 'RELAY_ACCESS_KEY=%s\nRELAY_MANAGEMENT_URL=%s\nRELAY_VERSION=%s\n' \
    "$access_key" "$management_url" "$version" > "$temporary"
  chown wenzwork-relay:wenzwork-relay "$temporary"
  chmod 0600 "$temporary"
  mv -f -- "$temporary" "$destination"
}

relay_update_env_version() {
  local file=$1 version=$2
  local temporary="${file}.new.$$"
  relay_validate_env_file "$file"
  [[ $version =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$ ]] || relay_die "Relay version is invalid"
  awk -v version="$version" '
    BEGIN { updated = 0 }
    $0 ~ /^RELAY_VERSION=/ { print "RELAY_VERSION=" version; updated = 1; next }
    { print }
    END { if (!updated) print "RELAY_VERSION=" version }
  ' "$file" > "$temporary"
  chown wenzwork-relay:wenzwork-relay "$temporary"
  chmod 0600 "$temporary"
  mv -f -- "$temporary" "$file"
}
