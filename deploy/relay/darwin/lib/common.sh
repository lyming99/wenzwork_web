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
  [[ $value =~ ^/[A-Za-z0-9._/-]+$ && $value != *//* && $value != */../* ]] ||
    relay_die "Relay install root must be a normalized absolute path without whitespace"
  case "$value" in
    /|/Applications|/Library|/System|/Users|/Volumes|/bin|/dev|/etc|/private|/sbin|/tmp|/usr|/var)
      relay_die "Relay install root is too broad: $value"
      ;;
  esac
  [[ ! -L $value ]] || relay_die "Relay install root must not be a symbolic link"
  printf '%s' "$value"
}

relay_installed_root() {
  local metadata=${RELAY_INSTALL_METADATA:-/Library/Application Support/WenzWork/Relay/install.conf} value=''
  if [[ -f $metadata && ! -L $metadata ]]; then
    value=$(awk -F= '$1 == "RELAY_INSTALL_ROOT" {print substr($0, index($0, "=") + 1); exit}' "$metadata")
  fi
  [[ -n $value ]] || value=/usr/local/lib/wenzwork-relay
  relay_validate_install_root "$value"
}

relay_write_install_metadata() {
  local install_root=$1 metadata=${RELAY_INSTALL_METADATA:-/Library/Application Support/WenzWork/Relay/install.conf}
  local temporary="${metadata}.new.$$"
  install_root=$(relay_validate_install_root "$install_root")
  printf 'RELAY_INSTALL_ROOT=%s\n' "$install_root" > "$temporary"
  chown root:wheel "$temporary"
  chmod 0600 "$temporary"
  mv -f "$temporary" "$metadata"
}

relay_normalize_architecture() {
  case "${1:-}" in
    x86_64|amd64) printf 'amd64' ;;
    arm64|aarch64) printf 'arm64' ;;
    *) relay_die "supported macOS Relay architectures are amd64/x86_64 and arm64" ;;
  esac
}

relay_host_platform() {
  [[ $(uname -s) == Darwin ]] || relay_die "this package requires macOS"
  printf 'darwin'
}

relay_host_architecture() { relay_normalize_architecture "$(uname -m)"; }

relay_preflight_host() {
  local install_root=${1:-/usr/local/lib/wenzwork-relay} major
  relay_require_command launchctl
  relay_require_command plutil
  relay_require_command curl
  relay_require_command tar
  relay_host_platform >/dev/null
  relay_host_architecture >/dev/null
  install_root=$(relay_validate_install_root "$install_root")
  major=$(sw_vers -productVersion | awk -F. '{print $1}')
  [[ $major =~ ^[0-9]+$ && $major -ge 13 ]] || relay_die "macOS 13 or newer is required"
  local disk_path=$install_root
  while [[ ! -e $disk_path && $disk_path != / ]]; do disk_path=$(dirname "$disk_path"); done
  local free_kib
  free_kib=$(df -Pk "$disk_path" | awk 'NR==2 {print $4}')
  [[ $free_kib =~ ^[0-9]+$ && $free_kib -ge 524288 ]] || relay_die "at least 512 MiB free space is required"
}

relay_download() {
  local url=$1 destination=$2
  relay_validate_url "$url"
  if [[ $url == https://* ]]; then
    curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
      --connect-timeout 10 --max-time 600 --output "$destination" "$url"
  else
    curl --fail --silent --show-error --location --proto '=http' --proto-redir '=http' \
      --connect-timeout 10 --max-time 600 --output "$destination" "$url"
  fi
}

relay_verify_bundle() {
  local archive=$1 checksums=$2 signature=$3 public_key=$4 verifier=$5
  [[ -x $verifier ]] || relay_die "a trusted executable relayctl verifier is required"
  "$verifier" release verify-bundle --archive "$archive" --checksums "$checksums" \
    --signature "$signature" --public-key "$public_key" >/dev/null ||
    relay_die "Relay archive signature or SHA-256 verification failed"
}

relay_verify_executable_sha256() {
  local executable=$1 expected=$2 actual
  [[ $expected =~ ^[0-9a-fA-F]{64}$ ]] || relay_die "trusted verifier SHA-256 is required"
  expected=$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')
  actual=$(shasum -a 256 "$executable" | awk '{print $1}')
  [[ $actual == "$expected" ]] || relay_die "bootstrap verifier SHA-256 mismatch"
}

relay_extract_bundle() {
  local archive=$1 destination=$2 entry listing entry_type
  while IFS= read -r listing; do
    entry_type=${listing:0:1}
    [[ $entry_type == '-' || $entry_type == d ]] || relay_die "archive contains a link or special file"
  done < <(LC_ALL=C tar -tvzf "$archive")
  while IFS= read -r entry; do
    [[ $entry != /* && $entry != ../* && $entry != */../* && $entry != *'/..' ]] ||
      relay_die "archive contains an unsafe path"
  done < <(tar -tzf "$archive")
  mkdir -p "$destination"
  tar -xzf "$archive" -C "$destination" --no-same-owner
}

relay_verify_release_tree() {
  local root=$1 version=$2 platform architecture
  platform=$(relay_host_platform)
  architecture=$(relay_host_architecture)
  [[ -x $root/bin/relayctl ]] || return 1
  "$root/bin/relayctl" release verify --root "$root" --manifest release-manifest.json \
    --expected-version "$version" --expected-platform "$platform" \
    --expected-architecture "$architecture" --protocol-version 1 >/dev/null
}

relay_atomic_symlink() {
  local target=$1 link=$2
  local temporary="${link}.new.$$"
  ln -s "$target" "$temporary"
  mv -fh "$temporary" "$link"
}

relay_install_release_tree() {
  local package_root=$1 release_dir=$2 version=$3 install_root=$4 stage
  [[ $release_dir == "$install_root/releases/$version" ]] || relay_die "invalid release destination"
  if [[ -d $release_dir ]]; then
    relay_verify_release_tree "$release_dir" "$version" || relay_die "existing release verification failed"
    return
  fi
  [[ ! -e $release_dir ]] || relay_die "release destination is not a directory"
  stage=$(mktemp -d "$install_root/releases/.release.XXXXXX")
  if ! cp -R "$package_root/." "$stage/" || ! chown -R root:wheel "$stage" ||
    ! find "$stage" -type d -exec chmod 0755 {} + || ! find "$stage" -type f -exec chmod 0644 {} + ||
    ! chmod 0755 "$stage/bin/wenzwork-relay-server" "$stage/bin/relayctl" "$stage/scripts/"*.sh ||
    ! relay_verify_release_tree "$stage" "$version"; then
    rm -rf -- "$stage"
    relay_die "could not stage the verified release"
  fi
  mv "$stage" "$release_dir"
}

relay_write_env() {
  local destination=$1 access_key=$2 management_url=$3 version=$4
  local temporary="${destination}.new.$$"
  [[ $access_key =~ ^relay_[A-Za-z0-9_-]{43}$ ]] || relay_die "Access Key is invalid"
  relay_validate_url "$management_url"
  [[ $version =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$ ]] || relay_die "Relay version is invalid"
  printf 'RELAY_ACCESS_KEY=%s\nRELAY_MANAGEMENT_URL=%s\nRELAY_VERSION=%s\n' \
    "$access_key" "$management_url" "$version" > "$temporary"
  chown root:wheel "$temporary"
  chmod 0600 "$temporary"
  mv -f "$temporary" "$destination"
}

relay_update_env_version() {
  local file=$1 version=$2
  local temporary="${file}.new.$$"
  [[ -f $file && ! -L $file ]] || relay_die "Relay environment is missing or unsafe"
  awk -v version="$version" '
    BEGIN { updated=0 }
    /^RELAY_VERSION=/ { print "RELAY_VERSION=" version; updated=1; next }
    { print }
    END { if (!updated) print "RELAY_VERSION=" version }
  ' "$file" > "$temporary"
  chown root:wheel "$temporary"
  chmod 0600 "$temporary"
  mv -f "$temporary" "$file"
}

relay_render_launchd_plist() {
  local source=$1 destination=$2 install_root=$3 env_file=$4
  local temporary="${destination}.new.$$"
  sed -e "s|__INSTALL_ROOT__|$install_root|g" -e "s|__ENV_FILE__|$env_file|g" "$source" > "$temporary"
  plutil -lint "$temporary" >/dev/null || relay_die "generated launchd plist is invalid"
  chown root:wheel "$temporary"
  chmod 0644 "$temporary"
  mv -f "$temporary" "$destination"
}

relay_launchd_reload() {
  local plist=$1
  launchctl bootout system/com.wenzwork.relay >/dev/null 2>&1 || true
  launchctl bootstrap system "$plist"
  launchctl enable system/com.wenzwork.relay
  launchctl kickstart -k system/com.wenzwork.relay
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
    [[ -f $key_file && ! -L $key_file ]] || relay_die "Access Key file is missing or unsafe"
    [[ $(stat -f '%Lp' "$key_file") == 600 ]] || relay_die "Access Key file permissions must be 0600"
    IFS= read -r key < "$key_file"
  fi
  [[ $key =~ ^relay_[A-Za-z0-9_-]{43}$ ]] || relay_die "Access Key is invalid"
  printf '%s' "$key"
}
