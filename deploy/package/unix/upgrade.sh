#!/usr/bin/env bash

set -Eeuo pipefail
umask 077

PACKAGE_ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=lib/common.sh
source "$PACKAGE_ROOT/runtime/lib/common.sh"

usage() {
  cat <<'EOF'
Usage:
  ./upgrade.sh
  ./upgrade.sh VERSION
  ./upgrade.sh PACKAGE SHA256SUMS

Without arguments, downloads the latest matching asset from work.wenzflow.com,
then wenzwork.com, then the public GitHub Release. VERSION selects one release
tag. A local PACKAGE must be accompanied by SHA256SUMS.
WENZWORK_OFFICIAL_RELEASE_BASE_URL can explicitly replace the two website
sources. It, GITHUB_RELEASE_REPOSITORY, and optional GITHUB_ACCESS_TOKEN are
read from .env.
The archive hash and target metadata are verified before the current code and
configuration are backed up. Mutable runtime, workspace, cache, and .env data
are preserved. Failed installation rolls the package files back.
EOF
}

case "${1:-}" in
  -h | --help | help)
    usage
    exit 0
    ;;
esac
(( $# <= 2 )) || {
  usage >&2
  exit 2
}

package_validate_tree "$PACKAGE_ROOT"
package_load_metadata "$PACKAGE_ROOT"
package_create_runtime_directories "$PACKAGE_ROOT"
command -v tar >/dev/null 2>&1 || package_die "tar is required for upgrades"

upgrade_progress() {
  local percent="$1"
  shift
  package_log "Progress ${percent}%: $*"
}

upgrade_progress 5 "Validated the installed $WENZWORK_PACKAGE_COMPONENT package."

temporary="$(mktemp -d "$PACKAGE_ROOT/runtime/.upgrade.XXXXXX")"
cleanup() {
  rm -rf -- "${temporary:?}"
}
trap cleanup EXIT HUP INT TERM

archive=''
checksums=''
requested="${1:-}"

download_official_release() {
  local official_base="$1" catalog_platform catalog_architecture metadata_url metadata record
  local archive_name expected_sha256 download_url download_target downloaded_archive downloaded_checksums
  official_base="${official_base%/}"
  case "$official_base" in
    https://* | http://localhost:* | http://127.0.0.1:* | http://\[::1\]:*) ;;
    *) return 1 ;;
  esac
  catalog_platform="$WENZWORK_PACKAGE_PLATFORM"
  [[ "$catalog_platform" != darwin ]] || catalog_platform=macos
  catalog_architecture="$WENZWORK_PACKAGE_ARCHITECTURE"
  [[ "$catalog_architecture" != amd64 ]] || catalog_architecture=x64
  if [[ -n "$requested" ]]; then
    metadata_url="$official_base/api/v1/releases?project=web&channel=stable&platform=$catalog_platform&architecture=$catalog_architecture&limit=50"
  else
    metadata_url="$official_base/api/v1/releases/latest?project=web&channel=stable&platform=$catalog_platform&architecture=$catalog_architecture"
  fi
  metadata="$temporary/official-release.json"
  package_try_download "$metadata_url" "$metadata" '' application/json || return 1
  record="$(package_catalog_asset_record "$metadata" "$WENZWORK_PACKAGE_ASSET_BASENAME" \
    "$WENZWORK_PACKAGE_PLATFORM" "$WENZWORK_PACKAGE_ARCHITECTURE" "$requested" 2>/dev/null)" || return 1
  IFS=$'\t' read -r archive_name expected_sha256 download_url <<< "$record"
  [[ "$expected_sha256" =~ ^[0-9a-f]{64}$ && -n "$archive_name" && -n "$download_url" ]] || return 1
  case "$download_url" in
    https://* | http://localhost:* | http://127.0.0.1:* | http://\[::1\]:*) download_target="$download_url" ;;
    /*) download_target="$official_base$download_url" ;;
    *) return 1 ;;
  esac
  downloaded_archive="$temporary/$archive_name"
  downloaded_checksums="$temporary/official-$WENZWORK_PACKAGE_CHECKSUM_ASSET"
  package_try_download "$download_target" "$downloaded_archive" || return 1
  printf '%s  %s\n' "$expected_sha256" "$archive_name" > "$downloaded_checksums"
  archive="$downloaded_archive"
  checksums="$downloaded_checksums"
  package_log "Downloaded $archive_name from $official_base."
}

download_github_api_release() {
  local repository="$1" token="$2" api release_json tag requested_tag safe_tag archive_name
  local archive_record checksums_record archive_api _archive_browser checksums_api _checksums_browser
  api="https://api.github.com/repos/$repository/releases/latest"
  if [[ -n "$requested" ]]; then
    requested_tag="$requested"
    if [[ "$requested_tag" =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?$ ]]; then
      requested_tag="v$requested_tag"
    fi
    api="https://api.github.com/repos/$repository/releases/tags/$requested_tag"
  fi
  release_json="$temporary/github-release.json"
  package_try_download "$api" "$release_json" "$token" application/vnd.github+json || return 1
  tag="$(package_json_tag_name "$release_json" 2>/dev/null || true)"
  [[ -n "$tag" ]] || return 1
  safe_tag="$(package_safe_version "$tag")"
  archive_name="$WENZWORK_PACKAGE_ASSET_BASENAME-$safe_tag-$WENZWORK_PACKAGE_PLATFORM-$WENZWORK_PACKAGE_ARCHITECTURE.tar.gz"
  archive_record="$(package_json_asset_record "$release_json" "$archive_name" 2>/dev/null || true)"
  checksums_record="$(package_json_asset_record "$release_json" "$WENZWORK_PACKAGE_CHECKSUM_ASSET" 2>/dev/null || true)"
  [[ -n "$archive_record" && -n "$checksums_record" ]] || return 1
  IFS=$'\t' read -r archive_api _archive_browser <<< "$archive_record"
  IFS=$'\t' read -r checksums_api _checksums_browser <<< "$checksums_record"
  archive="$temporary/$archive_name"
  checksums="$temporary/$WENZWORK_PACKAGE_CHECKSUM_ASSET"
  package_try_download "$archive_api" "$archive" "$token" || return 1
  package_try_download "$checksums_api" "$checksums" "$token" || return 1
  package_log "Downloaded $archive_name through the authenticated GitHub Release API."
}

download_public_github_release() {
  local repository="$1" tag release_path safe_tag archive_name matches
  tag="$requested"
  if [[ "$tag" =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?$ ]]; then
    tag="v$tag"
  fi
  if [[ -n "$tag" ]]; then
    safe_tag="$(package_safe_version "$tag")"
    archive_name="$WENZWORK_PACKAGE_ASSET_BASENAME-$safe_tag-$WENZWORK_PACKAGE_PLATFORM-$WENZWORK_PACKAGE_ARCHITECTURE.tar.gz"
    release_path="download/$tag"
  else
    release_path='latest/download'
  fi
  checksums="$temporary/$WENZWORK_PACKAGE_CHECKSUM_ASSET"
  package_try_download "https://github.com/$repository/releases/$release_path/$WENZWORK_PACKAGE_CHECKSUM_ASSET" "$checksums" || return 1
  if [[ -z "${archive_name:-}" ]]; then
    matches="$(awk -v prefix="$WENZWORK_PACKAGE_ASSET_BASENAME-" \
      -v suffix="-$WENZWORK_PACKAGE_PLATFORM-$WENZWORK_PACKAGE_ARCHITECTURE.tar.gz" '
      {
        candidate=$2
        sub(/^\*/, "", candidate)
        sub(/^\.\//, "", candidate)
        if (index(candidate, prefix) == 1 && substr(candidate, length(candidate)-length(suffix)+1) == suffix) print candidate
      }' "$checksums")"
    [[ "$matches" =~ ^[A-Za-z0-9._+-]+\.tar\.gz$ ]] || return 1
    archive_name="$matches"
  fi
  archive="$temporary/$archive_name"
  package_try_download "https://github.com/$repository/releases/$release_path/$archive_name" "$archive" || return 1
  package_log "Downloaded $archive_name from the public GitHub Release page."
}

if [[ -n "$requested" && -f "$requested" ]]; then
  (( $# == 2 )) || package_die "a local package requires a SHA256SUMS path"
  archive="$(CDPATH='' cd -- "$(dirname -- "$requested")" && pwd -P)/$(basename -- "$requested")"
  checksums_path="$2"
  [[ -f "$checksums_path" ]] || package_die "checksum file is missing: $checksums_path"
  checksums="$(CDPATH='' cd -- "$(dirname -- "$checksums_path")" && pwd -P)/$(basename -- "$checksums_path")"
  upgrade_progress 35 "Using the supplied local package and checksum file."
else
  repository="$(package_read_env_value "$PACKAGE_ROOT/.env" GITHUB_RELEASE_REPOSITORY 2>/dev/null || true)"
  [[ -n "$repository" ]] ||
    repository="$(package_metadata_value "$PACKAGE_ROOT" WENZWORK_GITHUB_REPOSITORY)"
  [[ "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] ||
    package_die "GITHUB_RELEASE_REPOSITORY must use owner/repository format"
  token="$(package_read_env_value "$PACKAGE_ROOT/.env" GITHUB_ACCESS_TOKEN 2>/dev/null || true)"
  if [[ -n "$requested" ]]; then
    [[ "$requested" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$ ]] ||
      package_die "invalid release tag: $requested"
  fi
  official_override="$(package_read_env_value "$PACKAGE_ROOT/.env" WENZWORK_OFFICIAL_RELEASE_BASE_URL 2>/dev/null || true)"
  if [[ -n "$official_override" ]]; then
    official_sources=("$official_override")
    missing_sources="${official_override%/} or github.com"
  else
    official_sources=(https://work.wenzflow.com https://wenzwork.com)
    missing_sources='work.wenzflow.com, wenzwork.com, or github.com'
  fi
  downloaded=false
  source_progress=10
  for official_source in "${official_sources[@]}"; do
    upgrade_progress "$source_progress" "Trying release source ${official_source%/}."
    if download_official_release "$official_source"; then
      downloaded=true
      break
    fi
    package_log "No matching upgrade package is available from ${official_source%/}; trying the next source."
    source_progress=$((source_progress + 10))
  done
  if [[ "$downloaded" == false ]]; then
    upgrade_progress 30 "Trying release source github.com."
    if [[ -n "$token" ]] && download_github_api_release "$repository" "$token"; then
      downloaded=true
    elif download_public_github_release "$repository"; then
      downloaded=true
    fi
  fi
  [[ "$downloaded" == true ]] ||
    package_die "upgrade package was not found at $missing_sources"
  upgrade_progress 35 "Downloaded a matching upgrade package."
fi

upgrade_progress 40 "Verifying the archive SHA-256 and path safety."
package_verify_archive_hash "$archive" "$checksums"
package_assert_safe_archive "$archive"
upgrade_progress 50 "Extracting and validating the target package."
stage="$temporary/stage"
mkdir -p "$stage"
tar -xzf "$archive" -C "$stage"
package_validate_tree "$stage"

next_component="$(package_metadata_value "$stage" WENZWORK_PACKAGE_COMPONENT)"
next_platform="$(package_metadata_value "$stage" WENZWORK_PACKAGE_PLATFORM)"
next_architecture="$(package_metadata_value "$stage" WENZWORK_PACKAGE_ARCHITECTURE)"
next_version="$(package_metadata_value "$stage" WENZWORK_PACKAGE_VERSION)"
[[ "$next_component" == "$WENZWORK_PACKAGE_COMPONENT" ]] ||
  package_die "package component mismatch: $next_component"
[[ "$next_platform" == "$WENZWORK_PACKAGE_PLATFORM" ]] ||
  package_die "package platform mismatch: $next_platform"
[[ "$next_architecture" == "$WENZWORK_PACKAGE_ARCHITECTURE" ]] ||
  package_die "package architecture mismatch: $next_architecture"
[[ -n "$next_version" ]] || package_die "new package version is empty"

upgrade_progress 65 "Backing up package files for $WENZWORK_PACKAGE_VERSION."
timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
backup="$PACKAGE_ROOT/cache/backups/${WENZWORK_PACKAGE_VERSION}_$timestamp"
mkdir -p "$backup"
for name in bin config runtime/lib web migrations .env start.sh stop.sh init.sh upgrade.sh backup.sh start.cmd Start.ps1 Stop.ps1 Init.ps1 Upgrade.ps1 Backup.ps1 VERSION PACKAGE-MANIFEST.json; do
  [[ -e "$PACKAGE_ROOT/$name" ]] || continue
  mkdir -p "$backup/$(dirname -- "$name")"
  cp -a "$PACKAGE_ROOT/$name" "$backup/$name"
done
package_log "Backed up $WENZWORK_PACKAGE_VERSION package files to $backup."

was_running=0
pid_file="$PACKAGE_ROOT/runtime/pids/wenzwork.pid"
if [[ -f "$pid_file" ]]; then
  pid="$(tr -d '[:space:]' < "$pid_file")"
  if [[ "$pid" =~ ^[1-9][0-9]*$ ]] && kill -0 "$pid" 2>/dev/null; then
    was_running=1
    upgrade_progress 75 "Stopping the running $WENZWORK_PACKAGE_COMPONENT service."
    "$PACKAGE_ROOT/stop.sh"
  fi
fi

restore_backup() {
  package_log "Restoring package files from $backup..."
  rm -rf -- "${PACKAGE_ROOT:?}/bin" "${PACKAGE_ROOT:?}/config" "${PACKAGE_ROOT:?}/runtime/lib" "${PACKAGE_ROOT:?}/web" "${PACKAGE_ROOT:?}/migrations"
  for name in .env start.sh stop.sh init.sh upgrade.sh backup.sh start.cmd Start.ps1 Stop.ps1 Init.ps1 Upgrade.ps1 Backup.ps1 VERSION PACKAGE-MANIFEST.json; do
    rm -f -- "$PACKAGE_ROOT/$name"
  done
  for name in bin config runtime/lib web migrations .env start.sh stop.sh init.sh upgrade.sh backup.sh start.cmd Start.ps1 Stop.ps1 Init.ps1 Upgrade.ps1 Backup.ps1 VERSION PACKAGE-MANIFEST.json; do
    [[ -e "$backup/$name" ]] || continue
    mkdir -p "$PACKAGE_ROOT/$(dirname -- "$name")"
    cp -a "$backup/$name" "$PACKAGE_ROOT/$name"
  done
}

install_stage() {
  rm -rf -- "${PACKAGE_ROOT:?}/bin" "${PACKAGE_ROOT:?}/runtime/lib" "${PACKAGE_ROOT:?}/web" "${PACKAGE_ROOT:?}/migrations"
  for name in bin runtime/lib web migrations; do
    [[ -e "$stage/$name" ]] || continue
    mkdir -p "$PACKAGE_ROOT/$(dirname -- "$name")"
    cp -a "$stage/$name" "$PACKAGE_ROOT/$name"
  done
  mkdir -p "$PACKAGE_ROOT/config"
  cp -a "$stage/config/." "$PACKAGE_ROOT/config/"
  for name in start.sh stop.sh init.sh upgrade.sh backup.sh start.cmd Start.ps1 Stop.ps1 Init.ps1 Upgrade.ps1 Backup.ps1 VERSION PACKAGE-MANIFEST.json; do
    rm -f -- "$PACKAGE_ROOT/$name"
  done
  for name in start.sh stop.sh init.sh upgrade.sh backup.sh start.cmd Start.ps1 Stop.ps1 Init.ps1 Upgrade.ps1 Backup.ps1 VERSION PACKAGE-MANIFEST.json; do
    [[ -e "$stage/$name" ]] || continue
    cp -a "$stage/$name" "$PACKAGE_ROOT/$name"
  done
  package_validate_tree "$PACKAGE_ROOT"
}

upgrade_progress 85 "Installing and validating package $next_version."
if ! install_stage; then
  restore_backup
  (( was_running == 0 )) || "$PACKAGE_ROOT/start.sh" --background
  package_die "upgrade failed; $WENZWORK_PACKAGE_VERSION was restored"
fi

upgrade_progress 95 "Applying the pre-upgrade service state."
if (( was_running == 1 )); then
  if ! "$PACKAGE_ROOT/start.sh" --background; then
    restore_backup
    "$PACKAGE_ROOT/start.sh" --background || true
    package_die "new version failed to start; $WENZWORK_PACKAGE_VERSION was restored"
  fi
fi
upgrade_progress 100 "Upgrade completed: $WENZWORK_PACKAGE_VERSION -> $next_version."
package_log "Upgrade completed: $WENZWORK_PACKAGE_VERSION -> $next_version"
