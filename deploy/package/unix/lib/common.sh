#!/usr/bin/env bash

set -Eeuo pipefail

package_log() {
  printf '[wenzwork-package] %s\n' "$*"
}

package_die() {
  package_log "ERROR: $*" >&2
  exit 1
}

package_trim() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

package_decode_env_value() {
  local value first last
  value="$(package_trim "$1")"
  if (( ${#value} >= 2 )); then
    first="${value:0:1}"
    last="${value: -1}"
    if [[ "$first" == '"' && "$last" == '"' ]]; then
      value="${value:1:${#value}-2}"
		value="$(package_decode_double_quoted_value "$value")"
	elif [[ "$first" == "'" && "$last" == "'" ]]; then
		value="${value:1:${#value}-2}"
    fi
  fi
  printf '%s' "$value"
}

package_decode_double_quoted_value() {
	local value="$1" output='' character next index length
	length="${#value}"
	for ((index = 0; index < length; index++)); do
		character="${value:index:1}"
		if [[ "$character" != "\\" ]]; then
			output+="$character"
			continue
		fi
		index=$((index + 1))
		(( index < length )) || package_die "invalid trailing escape in environment value"
		next="${value:index:1}"
		case "$next" in
			"\\" | "\"") output+="$next" ;;
			*) output+="\\$next" ;;
		esac
	done
	printf '%s' "$output"
}

package_read_env_value() {
  local file="$1" wanted="$2" line parsed key value found=0
  [[ -f "$file" ]] || return 1
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    parsed="$(package_trim "$line")"
    [[ -z "$parsed" || "$parsed" == \#* ]] && continue
    if [[ "$parsed" == export[[:space:]]* ]]; then
      parsed="$(package_trim "${parsed#export}")"
    fi
    [[ "$parsed" =~ ^([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*=(.*)$ ]] ||
      package_die "invalid environment entry in $file"
    key="${BASH_REMATCH[1]}"
    value="$(package_decode_env_value "${BASH_REMATCH[2]}")"
    if [[ "$key" == "$wanted" ]]; then
      printf '%s' "$value"
      found=1
    fi
  done < "$file"
  (( found == 1 ))
}

package_load_env() {
  local file="$1" line parsed key value
  [[ -f "$file" ]] || package_die "environment file is missing: $file"
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    parsed="$(package_trim "$line")"
    [[ -z "$parsed" || "$parsed" == \#* ]] && continue
    if [[ "$parsed" == export[[:space:]]* ]]; then
      parsed="$(package_trim "${parsed#export}")"
    fi
    [[ "$parsed" =~ ^([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*=(.*)$ ]] ||
      package_die "invalid environment entry in $file"
    key="${BASH_REMATCH[1]}"
    value="$(package_decode_env_value "${BASH_REMATCH[2]}")"
    export "$key=$value"
  done < "$file"
}

package_set_env_value() {
  local file="$1" wanted="$2" replacement="$3" temporary line parsed found=0
  temporary="$(mktemp "${file}.tmp.XXXXXX")"
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    parsed="$(package_trim "$line")"
    if [[ "$parsed" =~ ^([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*= ]] &&
      [[ "${BASH_REMATCH[1]}" == "$wanted" ]]; then
      if (( found == 0 )); then
        printf '%s=%s\n' "$wanted" "$replacement" >> "$temporary"
      fi
      found=1
    else
      printf '%s\n' "$line" >> "$temporary"
    fi
  done < "$file"
  if (( found == 0 )); then
    printf '\n%s=%s\n' "$wanted" "$replacement" >> "$temporary"
  fi
  chmod 0600 "$temporary"
  mv -f -- "$temporary" "$file"
}

package_metadata_value() {
  package_read_env_value "$1/config/package.env" "$2"
}

package_load_metadata() {
  local root="$1"
  package_load_env "$root/config/package.env"
  : "${WENZWORK_PACKAGE_COMPONENT:?package component is missing}"
  : "${WENZWORK_PACKAGE_PLATFORM:?package platform is missing}"
  : "${WENZWORK_PACKAGE_ARCHITECTURE:?package architecture is missing}"
  : "${WENZWORK_PACKAGE_VERSION:?package version is missing}"
  : "${WENZWORK_PACKAGE_ASSET_BASENAME:?package asset basename is missing}"
  : "${WENZWORK_PACKAGE_CHECKSUM_ASSET:?package checksum asset is missing}"
}

package_environment_template() {
  local root="$1" component="${2:-}"
  [[ -n "$component" ]] || component="$(package_metadata_value "$root" WENZWORK_PACKAGE_COMPONENT)"
  case "$component" in
    host) printf '%s/config/host.env.example' "$root" ;;
    relay) printf '%s/config/relay.env.example' "$root" ;;
    device-agent) printf '%s/config/device-agent.env.example' "$root" ;;
    *) package_die "unknown package component: $component" ;;
  esac
}

package_initialize_environment() {
  local root="$1" environment template temporary
  PACKAGE_ENVIRONMENT_CREATED=0
  environment="$root/.env"
  if [[ -e "$environment" || -L "$environment" ]]; then
    [[ -f "$environment" && ! -L "$environment" ]] ||
      package_die "environment path must be a regular file and must not be a symbolic link: $environment"
    chmod 0600 "$environment"
    return 0
  fi

  template="$(package_environment_template "$root")"
  [[ -f "$template" && ! -L "$template" ]] ||
    package_die "environment template must be a regular file and must not be a symbolic link: $template"
  temporary="$(mktemp "$root/.env.init.XXXXXX")" ||
    package_die "could not create a temporary environment file under $root"
  if ! cat -- "$template" > "$temporary" || ! chmod 0600 "$temporary"; then
    rm -f -- "$temporary"
    package_die "could not prepare .env from $template"
  fi
  if ln "$temporary" "$environment" 2>/dev/null; then
    rm -f -- "$temporary"
    # Read by start.sh after this helper returns.
    # shellcheck disable=SC2034
    PACKAGE_ENVIRONMENT_CREATED=1
    package_log "Created $environment from ${template#"$root/"}."
    return 0
  fi
  rm -f -- "$temporary"
  if [[ -f "$environment" && ! -L "$environment" ]]; then
    chmod 0600 "$environment"
    return 0
  fi
  package_die "could not create a safe environment file: $environment"
}

package_export_default() {
  local name="$1" value="$2"
  if [[ -z "${!name:-}" ]]; then
    printf -v "$name" '%s' "$value"
    export "${name?}"
  fi
}

package_apply_component_defaults() {
  local root="$1" environment=production public_base_url=https://wenzwork.com cookie_secure=false
  [[ "$WENZWORK_PACKAGE_COMPONENT" == host ]] || return 0
	package_export_default WENZWORK_ENV_FILE "$root/.env"
	package_export_default MIGRATIONS_DIR "$root/migrations"
  if [[ "${SYSTEM_SETUP_COMPLETED:-false}" != true ]] ||
	[[ -f "$root/config/relay-bootstrap/TEST_ONLY_SIGNING_KEY" ]]; then
    environment=development
    public_base_url=http://localhost:8080
    cookie_secure=false
  fi
  package_export_default APP_ENV "$environment"
  package_export_default PUBLIC_BASE_URL "$public_base_url"
  package_export_default HTTP_ADDR :8080
  package_export_default WEB_ROOT "$root/web"
  package_export_default LOG_LEVEL info
  package_export_default REGISTRATION_ENABLED true
  package_export_default COOKIE_SECURE "$cookie_secure"
  package_export_default ADMIN_MFA_REQUIRED false
  # PUBLIC_BASE_URL is assigned dynamically by package_export_default above.
  # shellcheck disable=SC2153
  package_export_default ALLOWED_ORIGINS "$PUBLIC_BASE_URL"
  package_export_default HOST_SECRETS_FILE "$root/cache/host-secrets/application.env"
  package_export_default RELEASE_ASSET_CACHE_DIR "$root/cache/releases"
  package_export_default GITHUB_RELEASE_REPOSITORY "$WENZWORK_GITHUB_REPOSITORY"
  package_export_default RELAY_DEVELOPMENT_CA_DIR "$root/cache/host-secrets/relay-ca"
  package_export_default RELAY_BOOTSTRAP_ASSETS_DIR "$root/config/relay-bootstrap"
  package_export_default REMOTE_MVP_ENABLED true
}

package_host_platform() {
  case "$(uname -s 2>/dev/null || true)" in
    Linux) printf 'linux' ;;
    Darwin) printf 'darwin' ;;
    MINGW* | MSYS* | CYGWIN*) printf 'windows' ;;
    *) package_die "unsupported operating system: $(uname -s 2>/dev/null || printf unknown)" ;;
  esac
}

package_host_architecture() {
  case "$(uname -m 2>/dev/null || true)" in
    x86_64 | amd64) printf 'amd64' ;;
    arm64 | aarch64) printf 'arm64' ;;
    *) package_die "unsupported architecture: $(uname -m 2>/dev/null || printf unknown)" ;;
  esac
}

package_binary_path() {
  local root="$1" name="$2" extension=''
  [[ "$(package_metadata_value "$root" WENZWORK_PACKAGE_PLATFORM)" != windows ]] || extension='.exe'
  printf '%s/bin/%s%s' "$root" "$name" "$extension"
}

package_required_binary() {
  case "$1" in
    host) printf 'wenzwork-api' ;;
    relay) printf 'wenzwork-relay-server' ;;
    device-agent) printf 'wenzwork-device-agent' ;;
    *) package_die "unknown package component: $1" ;;
  esac
}

package_assert_safe_root() {
  local root
  root="$(CDPATH='' cd -- "$1" 2>/dev/null && pwd -P)" ||
    package_die "package root does not exist: $1"
  [[ -n "$root" && "$root" != / && "$root" != "$HOME" ]] ||
    package_die "unsafe package root: $root"
  [[ -f "$root/config/package.env" ]] ||
    package_die "package metadata is missing under $root"
}

package_validate_tree() {
  local root="$1" component binary template
  package_assert_safe_root "$root"
  for directory in bin config runtime workspace cache; do
    [[ -d "$root/$directory" ]] || package_die "required directory is missing: $directory"
  done
  for file in start.sh upgrade.sh VERSION PACKAGE-MANIFEST.json; do
    [[ -f "$root/$file" ]] || package_die "required file is missing: $file"
  done
  component="$(package_metadata_value "$root" WENZWORK_PACKAGE_COMPONENT)"
  template="$(package_environment_template "$root" "$component")"
  [[ -f "$template" && ! -L "$template" ]] ||
    package_die "required environment template is missing or unsafe: ${template#"$root/"}"
  binary="$(package_required_binary "$component")"
  [[ -f "$(package_binary_path "$root" "$binary")" ]] ||
    package_die "required $component executable is missing"
}

package_create_runtime_directories() {
  local root="$1"
  mkdir -p "$root/logs" "$root/runtime/logs" "$root/runtime/pids" "$root/runtime/state" "$root/workspace" "$root/cache/backups"
}

package_sha256() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print tolower($1)}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print tolower($1)}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$file" | awk '{print tolower($NF)}'
  else
    package_die "sha256sum, shasum, or openssl is required"
  fi
}

package_verify_archive_hash() {
  local archive="$1" checksums="$2" name expected actual count
  name="$(basename -- "$archive")"
  [[ -f "$checksums" ]] || package_die "checksum file is missing: $checksums"
  count="$(awk -v wanted="$name" '
    {
      candidate=$2
      sub(/^\*/, "", candidate)
      sub(/^\.\//, "", candidate)
      if (candidate == wanted) {
        print tolower($1)
      }
    }
  ' "$checksums" | wc -l | tr -d '[:space:]')"
  [[ "$count" == 1 ]] || package_die "SHA256SUMS must contain exactly one entry for $name"
  expected="$(awk -v wanted="$name" '
    {
      candidate=$2
      sub(/^\*/, "", candidate)
      sub(/^\.\//, "", candidate)
      if (candidate == wanted) {
        print tolower($1)
      }
    }
  ' "$checksums")"
  [[ "$expected" =~ ^[0-9a-f]{64}$ ]] || package_die "invalid SHA-256 entry for $name"
  actual="$(package_sha256 "$archive")"
  [[ "$actual" == "$expected" ]] ||
    package_die "SHA-256 mismatch for $name (expected $expected, got $actual)"
  package_log "Verified SHA-256 for $name."
}

package_assert_safe_archive() {
  local archive="$1" entry listing_type
  while IFS= read -r entry; do
    entry="${entry#./}"
    [[ -z "$entry" ]] && continue
    case "$entry" in
      /* | ../* | */../* | */.. | *\\*) package_die "unsafe archive entry: $entry" ;;
    esac
  done < <(tar -tzf "$archive")
  while IFS= read -r listing_type; do
    case "$listing_type" in
      - | d) ;;
      *) package_die "archive contains a link or special file" ;;
    esac
  done < <(tar -tvzf "$archive" | sed -n 's/^\(.\).*$/\1/p')
}

package_try_download() {
  local url="$1" destination="$2" token="${3:-}" accept="${4:-application/octet-stream}" credentials_file=''
  case "$url" in
    https://* | http://localhost:* | http://127.0.0.1:* | http://\[::1\]:*) ;;
    *) return 1 ;;
  esac
  if [[ -n "$token" && ! "$token" =~ ^[A-Za-z0-9._-]+$ ]]; then
    return 1
  fi
  if command -v curl >/dev/null 2>&1; then
    local arguments=(--fail --location --progress-bar --show-error --retry 3 --output "$destination")
    arguments+=(--header "Accept: $accept" --header "X-GitHub-Api-Version: 2022-11-28")
    if [[ -n "$token" ]]; then
      credentials_file="$(mktemp "${destination}.curl.XXXXXX")" || return 1
      chmod 0600 "$credentials_file"
      printf 'header = "Authorization: Bearer %s"\n' "$token" > "$credentials_file"
      arguments+=(--config "$credentials_file")
    fi
    if ! curl "${arguments[@]}" "$url"; then
      rm -f -- "$destination"
      [[ -z "$credentials_file" ]] || rm -f -- "$credentials_file"
      return 1
    fi
  elif command -v wget >/dev/null 2>&1; then
    local arguments=(--progress=bar:force --tries=3 --output-document="$destination" --header="Accept: $accept" --header="X-GitHub-Api-Version: 2022-11-28")
    if [[ -n "$token" ]]; then
      credentials_file="$(mktemp "${destination}.wget.XXXXXX")" || return 1
      chmod 0600 "$credentials_file"
      printf 'header = Authorization: Bearer %s\n' "$token" > "$credentials_file"
    fi
    if [[ -n "$credentials_file" ]]; then
      if ! WGETRC="$credentials_file" wget "${arguments[@]}" "$url"; then
        rm -f -- "$destination" "$credentials_file"
        return 1
      fi
    elif ! wget "${arguments[@]}" "$url"; then
      rm -f -- "$destination"
      return 1
    fi
  else
    return 1
  fi
  [[ -z "$credentials_file" ]] || rm -f -- "$credentials_file"
  if [[ ! -s "$destination" ]]; then
    rm -f -- "$destination"
    return 1
  fi
}

package_download() {
  local url="$1" destination="$2" token="${3:-}" accept="${4:-application/octet-stream}"
  package_try_download "$url" "$destination" "$token" "$accept" ||
    package_die "could not download remote upgrade file: $url"
}

package_json_tag_name() {
  local file="$1"
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$file" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as stream:
    print(json.load(stream)["tag_name"])
PY
    return
  fi
  sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$file" | head -n 1
}

package_json_asset_record() {
  local file="$1" wanted="$2"
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$file" "$wanted" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as stream:
    release = json.load(stream)
matches = [asset for asset in release.get("assets", []) if asset.get("name") == sys.argv[2]]
if len(matches) != 1:
    raise SystemExit(1)
asset = matches[0]
print(f'{asset["url"]}\t{asset.get("browser_download_url", "")}')
PY
    return
  fi
  tr -d '[:space:]' < "$file" |
    sed 's/},{/}\n{/g' |
    awk -v wanted="\"name\":\"$wanted\"" 'index($0, wanted) {
      api=""
      browser=""
      if (match($0, /"url":"https:\/\/api\.github\.com\/repos\/[^"]+\/releases\/assets\/[0-9]+"/)) {
        api=substr($0, RSTART+7, RLENGTH-8)
      }
      if (match($0, /"browser_download_url":"https:\/\/[^"]+"/)) {
        browser=substr($0, RSTART+24, RLENGTH-25)
      }
      if (api != "") {
        print api "\t" browser
      }
    }'
}

package_catalog_asset_record() {
  local file="$1" basename="$2" platform="$3" architecture="$4" requested="${5:-}"
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$file" "$basename" "$platform" "$architecture" "$requested" <<'PY'
import json
import re
import sys

with open(sys.argv[1], encoding="utf-8") as stream:
    payload = json.load(stream)
releases = payload.get("items", []) if isinstance(payload, dict) and "items" in payload else [payload]
basename, platform, architecture, requested = sys.argv[2:]
suffix = f"-{platform}-{architecture}.tar.gz"
catalog_platform = "macos" if platform == "darwin" else platform
catalog_architecture = "x64" if architecture == "amd64" else architecture
safe_requested = re.sub(r"[^A-Za-z0-9._-]", "-", requested)
names = set()
if safe_requested:
    names.add(f"{basename}-{safe_requested}{suffix}")
    if re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?", safe_requested):
        names.add(f"{basename}-v{safe_requested}{suffix}")
matches = []
for release in releases:
    if not isinstance(release, dict):
        continue
    for asset in release.get("assets", []):
        name = asset.get("fileName", "")
        if asset.get("platform") != catalog_platform or asset.get("architecture") != catalog_architecture:
            continue
        if names:
            wanted = name in names
        else:
            wanted = name.startswith(basename + "-") and name.endswith(suffix)
        if wanted:
            matches.append(asset)
if len(matches) != 1:
    raise SystemExit(1)
asset = matches[0]
sha256 = str(asset.get("sha256", "")).lower()
url = str(asset.get("downloadUrl", ""))
name = str(asset.get("fileName", ""))
if not re.fullmatch(r"[0-9a-f]{64}", sha256) or not url or "\t" in url or "\t" in name:
    raise SystemExit(1)
print(f"{name}\t{sha256}\t{url}")
PY
    return
  fi
  tr -d '\r\n[:space:]' < "$file" |
    sed 's/},{/}\n{/g' |
    awk -v basename="$basename" -v platform="$platform" -v architecture="$architecture" -v requested="$requested" '
      function field(record, key, prefix, value) {
        prefix="\"" key "\":\""
        if (match(record, prefix "[^\"]*\"")) {
          value=substr(record, RSTART + length(prefix), RLENGTH - length(prefix) - 1)
          return value
        }
        return ""
      }
      {
        name=field($0, "fileName")
        sha=tolower(field($0, "sha256"))
        url=field($0, "downloadUrl")
        asset_platform=field($0, "platform")
        asset_architecture=field($0, "architecture")
        catalog_platform=(platform == "darwin" ? "macos" : platform)
        catalog_architecture=(architecture == "amd64" ? "x64" : architecture)
        suffix="-" platform "-" architecture ".tar.gz"
        matches=(asset_platform == catalog_platform && asset_architecture == catalog_architecture &&
          index(name, basename "-") == 1 && substr(name, length(name)-length(suffix)+1) == suffix)
        if (requested != "") {
          matches=(name == basename "-" requested suffix || name == basename "-v" requested suffix)
        }
        if (matches && sha ~ /^[0-9a-f]+$/ && length(sha) == 64 && url != "") {
          result=name "\t" sha "\t" url
          count++
        }
      }
      END {
        if (count != 1) exit 1
        print result
      }'
}

package_safe_version() {
  printf '%s' "$1" | sed 's/[^A-Za-z0-9._-]/-/g'
}
