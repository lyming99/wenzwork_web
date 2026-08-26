#!/usr/bin/env bash
set -euo pipefail

agent_maximum_archive_bytes=$((1024 * 1024 * 1024))
agent_maximum_archive_entries=512
agent_maximum_uncompressed_archive_bytes=$((1024 * 1024 * 1024))

agent_log() { printf '[wenzwork-device-agent] %s\n' "$*" >&2; }
agent_die() { agent_log "ERROR: $*"; exit 1; }

agent_require_root() {
  [[ ${EUID:-$(id -u)} -eq 0 ]] || agent_die "run this script with sudo or as root"
}

agent_require_command() {
  command -v "$1" >/dev/null 2>&1 || agent_die "required command is missing: $1"
}

agent_normalize_architecture() {
  case "${1:-}" in
    x86_64|amd64) printf 'amd64' ;;
    aarch64|arm64) printf 'arm64' ;;
    *) agent_die "supported Device Agent architectures are amd64/x86_64 and arm64/aarch64" ;;
  esac
}

agent_host_platform() {
  case "$(uname -s)" in
    Linux) printf 'linux' ;;
    Darwin) printf 'darwin' ;;
    *) agent_die "this installer supports Linux and macOS only" ;;
  esac
}

agent_host_architecture() {
  agent_normalize_architecture "$(uname -m)"
}

agent_validate_absolute_root() {
  local label=$1 value=${2:-} prefix='' remainder component
  [[ $value == / ]] || value=${value%/}
  [[ $value =~ ^/[A-Za-z0-9._/+@-]+([[:space:]][A-Za-z0-9._/+@-]+)*$ &&
      $value != *//* && $value != */./* && $value != */../* && $value != */. && $value != */.. ]] ||
    agent_die "$label must be a normalized absolute path"
  case "$value" in
    /|/bin|/boot|/dev|/etc|/home|/lib|/lib64|/Library|/opt|/private|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/var)
      agent_die "$label is too broad: $value"
      ;;
  esac
  remainder=${value#/}
  while [[ -n $remainder ]]; do
    component=${remainder%%/*}
    if [[ $remainder == */* ]]; then remainder=${remainder#*/}; else remainder=''; fi
    prefix="$prefix/$component"
    [[ ! -L $prefix ]] || agent_die "$label must not traverse a symbolic link: $prefix"
  done
  printf '%s' "$value"
}

agent_validate_install_root() {
  agent_validate_absolute_root 'Device Agent install root' "${1:-}"
}

agent_validate_data_root() {
  agent_validate_absolute_root 'Device Agent data root' "${1:-}"
}

agent_validate_config_root() {
  agent_validate_absolute_root 'Device Agent config root' "${1:-}"
}

agent_installed_root() {
  local metadata=$1 fallback=$2 value=''
  if [[ -f $metadata && ! -L $metadata ]]; then
    value=$(awk -F= '$1 == "WENZWORK_DEVICE_AGENT_INSTALL_ROOT" {print substr($0, index($0, "=") + 1); exit}' "$metadata")
  fi
  [[ -n $value ]] || value=$fallback
  agent_validate_install_root "$value"
}

agent_write_install_metadata() {
  local install_root=$1 metadata=$2 group=$3 temporary
  temporary="${metadata}.new.$$"
  install_root=$(agent_validate_install_root "$install_root")
  printf 'WENZWORK_DEVICE_AGENT_INSTALL_ROOT=%s\n' "$install_root" > "$temporary"
  chown root:"$group" "$temporary"
  chmod 0640 "$temporary"
  mv -f -- "$temporary" "$metadata"
}

agent_validate_url_authority() {
  local value=$1 allow_query=${2:-false} remainder authority
  [[ -n $value && ${#value} -le 2048 && $value != *[[:space:]]* && $value != *'#'* ]] ||
    agent_die "URL is invalid"
  [[ $allow_query == true || $value != *'?'* ]] || agent_die "Control Plane URL must not contain a query"
  case "$value" in
    https://*) remainder=${value#https://} ;;
    http://*) remainder=${value#http://} ;;
    *) agent_die "URL must use HTTP or HTTPS" ;;
  esac
  authority=${remainder%%/*}
  authority=${authority%%\?*}
  [[ -n $authority && $authority != *'@'* &&
      $authority =~ ^([A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?|\[[0-9A-Fa-f:]+\])(:[0-9]{1,5})?$ ]] ||
    agent_die "URL authority is invalid"
  printf '%s' "$authority"
}

agent_validate_url() {
  agent_validate_url_authority "$1" false >/dev/null
}

agent_validate_download_url() {
  local value=$1 authority
  authority=$(agent_validate_url_authority "$value" true)
  [[ $value == https://* ]] && return 0
  [[ $authority =~ ^(localhost|127\.0\.0\.1|\[::1\])(:[0-9]{1,5})?$ ]] ||
    agent_die "download URLs must use HTTPS (HTTP is allowed only for an exact loopback host)"
}

agent_download() {
  local url=$1 destination=$2
  agent_validate_download_url "$url"
  agent_require_command curl
  if [[ $url == https://* ]]; then
    curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
      --connect-timeout 10 --max-time 600 --max-filesize "$agent_maximum_archive_bytes" --output "$destination" "$url"
  else
    curl --fail --silent --show-error --location --proto '=http' --proto-redir '=http' \
      --connect-timeout 10 --max-time 600 --max-filesize "$agent_maximum_archive_bytes" --output "$destination" "$url"
  fi
}

agent_file_size() {
  local path=$1 size=''
  if size=$(stat -c '%s' "$path" 2>/dev/null); then
    :
  elif size=$(stat -f '%z' "$path" 2>/dev/null); then
    :
  else
    agent_die "could not determine file size"
  fi
  [[ $size =~ ^[0-9]+$ ]] || agent_die "file size is invalid"
  printf '%s' "$size"
}

agent_file_device() {
  local path=$1 device=''
  if device=$(stat -c '%d' "$path" 2>/dev/null); then
    :
  elif device=$(stat -f '%d' "$path" 2>/dev/null); then
    :
  else
    agent_die "could not determine filesystem device"
  fi
  [[ $device =~ ^[0-9]+$ ]] || agent_die "filesystem device identifier is invalid"
  printf '%s' "$device"
}

agent_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    agent_die "sha256sum or shasum is required"
  fi
}

agent_verify_executable_sha256() {
  local executable=$1 expected=${2:-} actual
  [[ -f $executable && ! -L $executable ]] || agent_die "trusted verifier must be a regular file"
  [[ $expected =~ ^[0-9a-fA-F]{64}$ ]] || agent_die "trusted verifier SHA-256 is required"
  actual=$(agent_sha256 "$executable")
  actual=$(printf '%s' "$actual" | tr '[:upper:]' '[:lower:]')
  expected=$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')
  [[ $actual == "$expected" ]] || agent_die "trusted verifier SHA-256 does not match"
}

agent_verify_bundle() {
  local archive=$1 checksums=$2 signature=$3 public_key=$4 verifier=${5:-}
  [[ -f $archive && ! -L $archive && -f $checksums && ! -L $checksums &&
      -f $signature && ! -L $signature && -f $public_key && ! -L $public_key ]] ||
    agent_die "archive, SHA256SUMS, signature, and trusted signing public key are required"
  if [[ -n $verifier ]]; then
    [[ -x $verifier && ! -L $verifier ]] || agent_die "trusted release verifier is not executable"
    "$verifier" release verify-bundle --archive "$archive" --checksums "$checksums" \
      --signature "$signature" --public-key "$public_key" >/dev/null ||
      agent_die "signed Device Agent bundle verification failed"
    return
  fi
  [[ $(agent_host_platform) == linux ]] ||
    agent_die "macOS installation requires a separately trusted --verifier-file"
  agent_require_command openssl
  agent_require_command sha256sum
  openssl pkeyutl -verify -pubin -inkey "$public_key" -rawin -in "$checksums" -sigfile "$signature" >/dev/null 2>&1 ||
    agent_die "SHA256SUMS signature verification failed"
  local archive_name expected_line
  archive_name=$(basename "$archive")
  expected_line=$(awk -v name="$archive_name" '$2 == name || $2 == "*" name {print; found=1} END {if (!found) exit 1}' "$checksums") ||
    agent_die "SHA256SUMS does not contain the selected Device Agent archive"
  (cd "$(dirname "$archive")" && printf '%s\n' "$expected_line" | sha256sum -c - >/dev/null) ||
    agent_die "Device Agent archive SHA-256 verification failed"
}

agent_verify_same_public_key() {
  local trusted=$1 packaged=$2 trusted_digest packaged_digest
  [[ -f $trusted && ! -L $trusted && -f $packaged && ! -L $packaged ]] ||
    agent_die "trusted and packaged Release public keys are required"
  trusted_digest=$(agent_sha256 "$trusted")
  packaged_digest=$(agent_sha256 "$packaged")
  [[ -n $trusted_digest && $trusted_digest == "$packaged_digest" ]] ||
    agent_die "packaged Release public key does not match the trusted key"
}

agent_extract_bundle() {
  local archive=$1 destination=$2 listing_file verbose_file listing entry_type entry
  local archive_bytes entry_count=0 metadata_count=0 probe_limit uncompressed_bytes
  agent_require_command tar
  agent_require_command head
  agent_require_command wc
  agent_require_command sort
  agent_require_command uniq
  [[ -f $archive && ! -L $archive ]] || agent_die "archive must be a regular file"
  archive_bytes=$(agent_file_size "$archive")
  (( archive_bytes <= agent_maximum_archive_bytes )) || agent_die "archive exceeds the 1 GiB safety limit"
  [[ ! -e $destination && ! -L $destination ]] || agent_die "archive destination already exists"
  listing_file="${destination}.entries.$$"
  verbose_file="${destination}.metadata.$$"
  LC_ALL=C tar -tzf "$archive" > "$listing_file" || agent_die "archive entry listing failed"
  LC_ALL=C tar -tvzf "$archive" > "$verbose_file" || agent_die "archive metadata listing failed"
  while IFS= read -r listing; do
    ((entry_count += 1))
    (( entry_count <= agent_maximum_archive_entries )) || agent_die "archive contains too many entries"
    entry_type=${listing:0:1}
    [[ $entry_type == '-' || $entry_type == d ]] ||
      agent_die "archive contains an unsupported link or special file"
  done < "$verbose_file"
  while IFS= read -r entry; do
    ((metadata_count += 1))
    [[ $entry != /* && $entry != ../* && $entry != */../* && $entry != *'/..' && $entry != *\\* ]] ||
      agent_die "archive contains an unsafe path"
  done < "$listing_file"
  (( entry_count > 0 && metadata_count == entry_count )) || agent_die "archive entry metadata is inconsistent"
  if LC_ALL=C sort "$listing_file" | uniq -d | grep -q .; then
    agent_die "archive contains a duplicate path"
  fi
  if [[ $(uname -s) == Darwin ]] &&
    LC_ALL=C tr '[:upper:]' '[:lower:]' < "$listing_file" | sort | uniq -d | grep -q .; then
    agent_die "archive contains paths that collide on a case-insensitive filesystem"
  fi
  probe_limit=$((agent_maximum_uncompressed_archive_bytes + 1))
  uncompressed_bytes=$(
    set +o pipefail
    LC_ALL=C tar -xOzf "$archive" 2>/dev/null | head -c "$probe_limit" | wc -c
  )
  uncompressed_bytes=${uncompressed_bytes//[[:space:]]/}
  [[ $uncompressed_bytes =~ ^[0-9]+$ ]] || agent_die "archive uncompressed size is invalid"
  (( uncompressed_bytes <= agent_maximum_uncompressed_archive_bytes )) ||
    agent_die "archive uncompressed size exceeds the 1 GiB safety limit"
  rm -f -- "$listing_file" "$verbose_file"
  mkdir -p "$destination"
  tar -xzf "$archive" -C "$destination" --no-same-owner --no-same-permissions
}

agent_find_package_root() {
  local extracted=$1 candidate='' found=''
  if [[ -x $extracted/bin/wenzwork-device-agent ]]; then
    printf '%s' "$extracted"
    return
  fi
  while IFS= read -r candidate; do
    [[ -z $found ]] || agent_die "archive must contain exactly one Device Agent package root"
    found=$candidate
  done < <(find "$extracted" -mindepth 1 -maxdepth 1 -type d -print)
  [[ -n $found && -x $found/bin/wenzwork-device-agent ]] ||
    agent_die "archive does not contain wenzwork-device-agent"
  printf '%s' "$found"
}

agent_assert_package_complete() {
  local root=$1 platform=$2 required
  for required in bin/wenzwork-device-agent bin/relayctl scripts/install.sh scripts/upgrade.sh \
    scripts/healthcheck.sh scripts/uninstall.sh scripts/lib/common.sh VERSION release-manifest.json \
    release-signing-public-key.pem device-agent.env.example; do
    [[ -f $root/$required && ! -L $root/$required ]] || agent_die "package is missing regular file $required"
  done
  if [[ $platform == linux ]]; then
    [[ -f $root/systemd/wenzwork-device-agent.service && ! -L $root/systemd/wenzwork-device-agent.service ]] ||
      agent_die "Linux package is missing its systemd unit"
  else
    [[ -f $root/launchd/com.wenzwork.device-agent.plist && ! -L $root/launchd/com.wenzwork.device-agent.plist ]] ||
      agent_die "macOS package is missing its launchd plist"
    agent_require_command codesign
    agent_require_command spctl
    codesign --verify --strict --verbose=2 "$root/bin/wenzwork-device-agent" >/dev/null 2>&1 ||
      agent_die "macOS Device Agent Developer ID signature is invalid"
    codesign --verify --strict --verbose=2 "$root/bin/relayctl" >/dev/null 2>&1 ||
      agent_die "macOS release verifier Developer ID signature is invalid"
    spctl --assess --type execute --verbose=2 "$root/bin/wenzwork-device-agent" >/dev/null 2>&1 ||
      agent_die "macOS Device Agent is not accepted by Gatekeeper/notarization policy"
    spctl --assess --type execute --verbose=2 "$root/bin/relayctl" >/dev/null 2>&1 ||
      agent_die "macOS release verifier is not accepted by Gatekeeper/notarization policy"
  fi
}

agent_verify_release_tree() {
  local root=$1 version=$2 platform architecture
  [[ -x $root/bin/relayctl && ! -L $root/bin/relayctl ]] || return 1
  platform=$(agent_host_platform)
  architecture=$(agent_host_architecture)
  "$root/bin/relayctl" release verify --root "$root" --manifest release-manifest.json \
    --expected-version "$version" --expected-platform "$platform" \
    --expected-architecture "$architecture" --protocol-version 1 >/dev/null
}

agent_install_release_tree() {
  local package_root=$1 release_dir=$2 version=$3 install_root=$4 releases_root stage=''
  install_root=$(agent_validate_install_root "$install_root")
  releases_root="$install_root/releases"
  [[ $release_dir == "$releases_root/$version" ]] || agent_die "release destination is invalid"
  [[ ! -L $release_dir ]] || agent_die "release destination must not be a symbolic link"
  if [[ -d $release_dir ]]; then
    agent_verify_release_tree "$release_dir" "$version" || agent_die "existing release failed manifest verification"
    return
  fi
  [[ ! -e $release_dir ]] || agent_die "release destination is not a directory"
  stage=$(mktemp -d "$releases_root/.release.XXXXXX")
  if ! {
    cp -a -- "$package_root/." "$stage/" &&
      chown -R root:root "$stage" &&
      find "$stage" -type d -exec chmod 0755 {} + &&
      find "$stage" -type f -exec chmod 0644 {} + &&
      chmod 0755 "$stage/bin/wenzwork-device-agent" "$stage/bin/relayctl" "$stage/scripts/"*.sh &&
      agent_verify_release_tree "$stage" "$version"
  }; then
    agent_remove_release_stage "$stage" "$releases_root"
    agent_die "could not stage the verified Device Agent release"
  fi
  if ! mv -- "$stage" "$release_dir"; then
    agent_remove_release_stage "$stage" "$releases_root"
    agent_die "could not install the Device Agent release"
  fi
}

agent_remove_release_stage() {
  local target=${1:-} releases_root=${2:-}
  [[ -n $target && -n $releases_root && $target == "$releases_root"/.release.?????? && $releases_root == /*/releases ]] || {
    agent_log "refusing to remove unexpected release stage: $target"
    return 1
  }
  rm -rf -- "$target"
}

agent_atomic_symlink() {
  local target=$1 link=$2 temporary
  temporary="${link}.new.$$"
  ln -s "$target" "$temporary"
  if [[ $(uname -s) == Darwin ]]; then
    mv -fh -- "$temporary" "$link"
  else
    mv -Tf -- "$temporary" "$link"
  fi
}

agent_validate_env_file() {
  local path=$1 expected_state=$2 key value state_seen=0 control_seen=0 access_seen=0
  [[ -f $path && ! -L $path ]] || agent_die "Device Agent environment file must be a regular file"
  while IFS= read -r line || [[ -n $line ]]; do
    [[ -z $line || $line =~ ^[[:space:]]*# ]] && continue
    [[ $line =~ ^([A-Z0-9_]+)=(.*)$ ]] || agent_die "Device Agent environment contains an invalid line"
    key=${BASH_REMATCH[1]}
    value=${BASH_REMATCH[2]}
    [[ $value != *$'\n'* && $value != *$'\r'* ]] || agent_die "Device Agent environment value is invalid"
    case "$key" in
      WENZWORK_CONTROL_URL)
        ((control_seen += 1)); agent_validate_url "$value" ;;
      WENZWORK_DEVICE_ACCESS_KEY)
        ((access_seen += 1)); [[ $value =~ ^device_[A-Za-z0-9_-]{43}$ ]] || agent_die "Device Access Key is invalid" ;;
      WENZWORK_DEVICE_STATE_FILE)
        ((state_seen += 1)); [[ $value == "$expected_state" ]] || agent_die "state file must be $expected_state" ;;
      WENZWORK_DEVICE_WORKSPACE|WENZWORK_AGENT_SECRET_STORE|WENZWORK_AGENT_FEATURE_FLAGS|WENZWORK_DEVICE_TLS_CA_FILE)
        ;;
      *) agent_die "Device Agent environment key is not allowed: $key" ;;
    esac
  done < "$path"
  [[ $control_seen -eq 1 && $access_seen -eq 1 && $state_seen -eq 1 ]] ||
    agent_die "environment must contain one Control URL, Device Access Key, and managed state path"
  [[ $(grep -Ec '^WENZWORK_AGENT_SECRET_STORE=file$' "$path") -eq 1 ]] ||
    agent_die "service packages require the encrypted file SecretStore so backups are complete and portable"
}

agent_write_env_file() {
  local source=$1 destination=$2 owner=$3 expected_state=$4 temporary
  temporary="${destination}.new.$$"
  agent_validate_env_file "$source" "$expected_state"
  install -m 0600 "$source" "$temporary"
  chown "$owner" "$temporary"
  mv -f -- "$temporary" "$destination"
}

agent_render_template() {
  local source=$1 destination=$2 install_root=$3 env_file=$4 temporary
  temporary="${destination}.new.$$"
  sed -e "s|__INSTALL_ROOT__|$install_root|g" -e "s|__ENV_FILE__|$env_file|g" "$source" > "$temporary"
  chown root:root "$temporary"
  chmod 0644 "$temporary"
  mv -f -- "$temporary" "$destination"
}

agent_assert_regular_data_tree() {
  local root=$1 label=$2 unsafe='' hardlink='' root_device candidate candidate_device
  [[ -d $root && ! -L $root ]] || agent_die "$label must be a real directory"
  unsafe=$(find "$root" -xdev -mindepth 1 \( -type l -o \! -type f \! -type d \) -print -quit)
  [[ -z $unsafe ]] || agent_die "$label contains a link or special file"
  hardlink=$(find "$root" -xdev -mindepth 1 -type f -links +1 -print -quit)
  [[ -z $hardlink ]] || agent_die "$label contains a multiply linked file"
  root_device=$(agent_file_device "$root")
  while IFS= read -r -d '' candidate; do
    candidate_device=$(agent_file_device "$candidate")
    [[ $candidate_device == "$root_device" ]] || agent_die "$label contains a filesystem mount point"
  done < <(find "$root" -xdev -mindepth 1 -type d -print0)
}

agent_assert_atomic_restore_layout() {
  local data_root=$1 parent data_device parent_device
  data_root=$(agent_validate_data_root "$data_root")
  [[ -d $data_root && ! -L $data_root ]] || agent_die "managed Agent data must be a real directory"
  parent=$(dirname -- "$data_root")
  data_device=$(agent_file_device "$data_root")
  parent_device=$(agent_file_device "$parent")
  [[ $data_device == "$parent_device" ]] ||
    agent_die "Device Agent data root must not be a filesystem mount point; mount its parent so rollback staging stays on one filesystem"
}

agent_create_backup() {
  local data_root=$1 env_file=$2 backup_root=$3 version=$4 backup_dir stage
  data_root=$(agent_validate_data_root "$data_root")
  backup_root=$(agent_validate_absolute_root 'Device Agent backup root' "$backup_root")
  [[ -d $data_root && ! -L $data_root && -f $env_file && ! -L $env_file ]] ||
    agent_die "managed data and environment must exist before backup"
  agent_assert_regular_data_tree "$data_root" 'managed Agent data'
  mkdir -p "$backup_root"
  chmod 0700 "$backup_root"
  stage=$(mktemp -d "$backup_root/.backup.XXXXXX")
  backup_dir="$backup_root/$(date -u +%Y%m%dT%H%M%SZ)-$version"
  [[ ! -e $backup_dir ]] || backup_dir="$backup_dir-$$"
  mkdir -p "$stage/data" "$stage/config"
  cp -a -x -- "$data_root/." "$stage/data/"
  cp -p -- "$env_file" "$stage/config/agent.env"
  agent_assert_regular_data_tree "$stage/data" 'staged Agent backup'
  printf 'schemaVersion=1\nsourceVersion=%s\ncreatedAt=%s\n' "$version" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$stage/BACKUP-METADATA"
  chmod 0600 "$stage/config/agent.env" "$stage/BACKUP-METADATA"
  mv -- "$stage" "$backup_dir"
  printf '%s' "$backup_dir"
}

agent_restore_backup() {
  local backup_dir=$1 data_root=$2 env_file=$3 owner=$4 backup_root=$5 backup_name stamp
  local failed_root restore_root restore_failed env_restore preserved=false
  backup_dir=$(agent_validate_absolute_root 'Device Agent backup path' "$backup_dir")
  backup_root=$(agent_validate_absolute_root 'Device Agent backup root' "$backup_root")
  case "$backup_dir" in
    "$backup_root"/*) backup_name=${backup_dir#"$backup_root"/} ;;
    *) agent_die "backup path is outside the managed backup root" ;;
  esac
  [[ -n $backup_name && $backup_name != */* ]] || agent_die "backup path must be a direct child of the managed backup root"
  data_root=$(agent_validate_data_root "$data_root")
  agent_assert_atomic_restore_layout "$data_root"
  [[ -d $backup_dir/data && ! -L $backup_dir/data &&
      -f $backup_dir/config/agent.env && ! -L $backup_dir/config/agent.env &&
      -f $backup_dir/BACKUP-METADATA && ! -L $backup_dir/BACKUP-METADATA ]] ||
    agent_die "backup is incomplete"
  agent_assert_regular_data_tree "$backup_dir/data" 'Agent backup data'
  agent_validate_env_file "$backup_dir/config/agent.env" "$data_root/state/agent-state.json"
  stamp="$(date -u +%Y%m%dT%H%M%SZ).$$"
  failed_root="${data_root}.failed.$stamp"
  restore_root="${data_root}.restore.$stamp"
  restore_failed="${data_root}.restore-failed.$stamp"
  env_restore="${env_file}.restore.$stamp"
  [[ ! -e $failed_root && ! -L $failed_root && ! -e $restore_root && ! -L $restore_root &&
      ! -e $restore_failed && ! -L $restore_failed && ! -e $env_restore && ! -L $env_restore ]] ||
    agent_die "rollback staging path already exists"

  mkdir "$restore_root"
  chmod 0700 "$restore_root"
  cp -a -x -- "$backup_dir/data/." "$restore_root/"
  agent_assert_regular_data_tree "$restore_root" 'staged Agent restore'
  chown -R "$owner" "$restore_root"
  chmod 0700 "$restore_root"
  install -m 0600 "$backup_dir/config/agent.env" "$env_restore"
  chown "$owner" "$env_restore"

  if [[ -e $data_root || -L $data_root ]]; then
    mv -- "$data_root" "$failed_root"
    preserved=true
  fi
  if ! mv -- "$restore_root" "$data_root"; then
    if [[ $preserved == true ]] && ! mv -- "$failed_root" "$data_root"; then
      agent_die "could not activate the staged restore or put the original data back"
    fi
    agent_die "could not activate the staged restore; original data was left in place"
  fi
  agent_assert_regular_data_tree "$data_root" 'restored Agent data'
  if ! mv -f -- "$env_restore" "$env_file"; then
    if ! mv -- "$data_root" "$restore_failed"; then
      agent_die "environment restore failed and restored data could not be moved aside"
    fi
    if [[ $preserved == true ]] && ! mv -- "$failed_root" "$data_root"; then
      agent_die "environment restore failed and original data could not be put back"
    fi
    agent_die "environment restore failed; original data was put back"
  fi
  if [[ $preserved == true ]]; then
    agent_log "failed-upgrade data retained at $failed_root for diagnostics"
  fi
}

agent_prune_backups() {
  local backup_root=$1 keep=${2:-5} candidate count=0
  backup_root=$(agent_validate_absolute_root 'Device Agent backup root' "$backup_root")
  [[ $keep =~ ^[1-9][0-9]*$ ]] || agent_die "backup retention must be positive"
  [[ -d $backup_root && ! -L $backup_root ]] || return 0
  while IFS= read -r candidate; do
    ((count += 1))
    (( count <= keep )) && continue
    [[ $candidate == "$backup_root"/* && -f $candidate/BACKUP-METADATA && ! -L $candidate ]] ||
      agent_die "refusing to prune an unexpected backup path"
    rm -rf -- "$candidate"
  done < <(find "$backup_root" -mindepth 1 -maxdepth 1 -type d ! -name '.backup.*' -print | LC_ALL=C sort -r)
}

agent_remove_temp_tree() {
  local target=${1:-}
  case "$target" in
    /tmp/wenzwork-device-agent-install.??????|/tmp/wenzwork-device-agent-upgrade.??????)
      rm -rf -- "$target"
      ;;
    *) agent_log "refusing to remove unexpected temporary path: $target"; return 1 ;;
  esac
}

agent_prepare_package() {
  local archive=$1 archive_url=$2 checksums=$3 checksums_url=$4 signature=$5 signature_url=$6
  local signing_key=$7 verifier=$8 work_dir=$9
  if [[ -n $archive_url ]]; then
    [[ -z $archive ]] || agent_die "select either --package-file or --artifact-url"
    [[ -n $checksums_url && -n $signature_url ]] || agent_die "URL installation requires checksum and signature URLs"
    local archive_name
    archive_name=$(basename "${archive_url%%\?*}")
    [[ $archive_name == *.tar.gz && $archive_name != */* ]] || agent_die "artifact URL must end with a safe .tar.gz file name"
    archive="$work_dir/$archive_name"
    checksums="$work_dir/SHA256SUMS"
    signature="$work_dir/SHA256SUMS.sig"
    agent_download "$archive_url" "$archive"
    agent_download "$checksums_url" "$checksums"
    agent_download "$signature_url" "$signature"
  fi
  [[ -n $archive ]] || agent_die "a signed Device Agent package is required"
  agent_verify_bundle "$archive" "$checksums" "$signature" "$signing_key" "$verifier"
  agent_extract_bundle "$archive" "$work_dir/package"
  local package_root platform version
  package_root=$(agent_find_package_root "$work_dir/package")
  platform=$(agent_host_platform)
  agent_assert_package_complete "$package_root" "$platform"
  agent_verify_same_public_key "$signing_key" "$package_root/release-signing-public-key.pem"
  version=$(tr -d '[:space:]' < "$package_root/VERSION")
  [[ $version =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$ ]] || agent_die "package VERSION is invalid"
  agent_verify_release_tree "$package_root" "$version" || agent_die "Device Agent release manifest verification failed"
  printf '%s\n%s\n' "$package_root" "$version"
}
