#!/usr/bin/env bash

set -Eeuo pipefail
umask 027

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${WENZWORK_ENV_FILE:-$SCRIPT_DIR/.env}"
START_SCRIPT="$SCRIPT_DIR/start_server.sh"
MEMINFO_FILE="${WENZWORK_MEMINFO_FILE:-/proc/meminfo}"
FSTAB_FILE="${WENZWORK_FSTAB_FILE:-/etc/fstab}"
SWAP_FILE="${WENZWORK_SWAP_FILE:-/swapfile}"
SWAP_SIZE="${WENZWORK_SWAP_SIZE:-1G}"
GOMEMLIMIT_VALUE="${WENZWORK_CONFIG_GOMEMLIMIT:-256MiB}"
MEMORY_HIGH_VALUE="${WENZWORK_CONFIG_MEMORY_HIGH:-384M}"
MEMORY_MAX_VALUE="${WENZWORK_CONFIG_MEMORY_MAX:-512M}"
SKIP_SWAP=0
SKIP_SYSTEMD=0
ENV_TEMP=""
SWAP_TEMP=""

log() {
  printf '[wenzwork-memory] %s\n' "$*"
}

fail() {
  log "ERROR: $*" >&2
  exit 1
}

cleanup() {
  if [[ -n "$ENV_TEMP" && -f "$ENV_TEMP" ]]; then
    rm -f -- "$ENV_TEMP"
  fi
  if [[ -n "$SWAP_TEMP" && -f "$SWAP_TEMP" ]]; then
    rm -f -- "$SWAP_TEMP"
  fi
}

trap cleanup EXIT

usage() {
  cat <<'EOF'
Usage: sudo ./configure_server_memory.sh [options]

Configure the recommended WenzWork memory profile for a 2 GiB Linux host:
  GOMEMLIMIT=256MiB
  WENZWORK_MEMORY_HIGH=384M
  WENZWORK_MEMORY_MAX=512M

When the host has no active swap, the script also creates a 1 GiB /swapfile,
persists it in /etc/fstab, and then installs or refreshes the systemd service.
The operation is idempotent and backs up files before changing them.

Options:
  --no-swap       Do not create or enable swap
  --no-systemd    Update memory configuration without installing systemd
  -h, --help      Show this help

Advanced overrides:
  WENZWORK_SWAP_SIZE=2G
  WENZWORK_SWAP_FILE=/swapfile
  WENZWORK_CONFIG_GOMEMLIMIT=256MiB
  WENZWORK_CONFIG_MEMORY_HIGH=384M
  WENZWORK_CONFIG_MEMORY_MAX=512M
EOF
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "Required command not found: $1"
}

backup_path() {
  local source="$1"
  local timestamp candidate
  timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
  candidate="$source.wenzwork-backup.$timestamp"
  if [[ -e "$candidate" ]]; then
    candidate="$candidate.$$"
  fi
  printf '%s\n' "$candidate"
}

validate_inputs() {
  [[ "$GOMEMLIMIT_VALUE" =~ ^[1-9][0-9]*(KiB|MiB|GiB|TiB)$ ]] ||
    fail "WENZWORK_CONFIG_GOMEMLIMIT must look like 256MiB."
  [[ "$MEMORY_HIGH_VALUE" =~ ^[1-9][0-9]*[KMGT]$ ]] ||
    fail "WENZWORK_CONFIG_MEMORY_HIGH must look like 384M."
  [[ "$MEMORY_MAX_VALUE" =~ ^[1-9][0-9]*[KMGT]$ ]] ||
    fail "WENZWORK_CONFIG_MEMORY_MAX must look like 512M."
  [[ "$SWAP_SIZE" =~ ^[1-9][0-9]*[MG]$ ]] ||
    fail "WENZWORK_SWAP_SIZE must look like 1G or 512M."
  [[ "$SWAP_FILE" == /* && "$SWAP_FILE" != "/" && "$SWAP_FILE" != *[[:space:]#]* ]] ||
    fail "WENZWORK_SWAP_FILE must be an absolute path without whitespace or #."
  [[ "$FSTAB_FILE" == /* && "$FSTAB_FILE" != "/" ]] ||
    fail "WENZWORK_FSTAB_FILE must be an absolute file path."
}

report_host_memory() {
  [[ -r "$MEMINFO_FILE" ]] || fail "Cannot read memory information: $MEMINFO_FILE"
  local total_kib swap_kib
  total_kib="$(awk '/^MemTotal:/ { print $2; exit }' "$MEMINFO_FILE")"
  swap_kib="$(awk '/^SwapTotal:/ { print $2; exit }' "$MEMINFO_FILE")"
  [[ "$total_kib" =~ ^[0-9]+$ && "$total_kib" -gt 0 ]] ||
    fail "MemTotal is missing from $MEMINFO_FILE."
  [[ "$swap_kib" =~ ^[0-9]+$ ]] || swap_kib=0
  log "Detected memory: $((total_kib / 1024)) MiB RAM, $((swap_kib / 1024)) MiB configured swap."
  if (( total_kib < 1572864 )); then
    log "WARNING: less than 1.5 GiB RAM is visible; verify that the ECS resize has taken effect."
  elif (( total_kib > 3145728 )); then
    log "NOTE: this conservative profile is intended for a 2 GiB host."
  fi
}

configure_env() {
  [[ -f "$ENV_FILE" ]] ||
    fail "Missing $ENV_FILE. Run ./init_server.sh and finish the production settings first."
  require_command awk
  require_command cmp
  require_command cp
  require_command mktemp

  ENV_TEMP="$(mktemp "$SCRIPT_DIR/.env.memory.XXXXXX")"
  cp -p "$ENV_FILE" "$ENV_TEMP"
  awk \
    -v go_limit="$GOMEMLIMIT_VALUE" \
    -v memory_high="$MEMORY_HIGH_VALUE" \
    -v memory_max="$MEMORY_MAX_VALUE" '
      function is_setting(line, key) {
        return line ~ "^[[:space:]]*(export[[:space:]]+)?" key "[[:space:]]*="
      }
      is_setting($0, "GOMEMLIMIT") {
        if (!seen_go++) print "GOMEMLIMIT=" go_limit
        next
      }
      is_setting($0, "WENZWORK_MEMORY_HIGH") {
        if (!seen_high++) print "WENZWORK_MEMORY_HIGH=" memory_high
        next
      }
      is_setting($0, "WENZWORK_MEMORY_MAX") {
        if (!seen_max++) print "WENZWORK_MEMORY_MAX=" memory_max
        next
      }
      { print }
      END {
        if (!seen_go) print "GOMEMLIMIT=" go_limit
        if (!seen_high) print "WENZWORK_MEMORY_HIGH=" memory_high
        if (!seen_max) print "WENZWORK_MEMORY_MAX=" memory_max
      }
    ' "$ENV_FILE" > "$ENV_TEMP"

  if cmp -s "$ENV_FILE" "$ENV_TEMP"; then
    rm -f -- "$ENV_TEMP"
    ENV_TEMP=""
    log "Application memory settings are already current."
    return
  fi

  local backup
  backup="$(backup_path "$ENV_FILE")"
  cp -p "$ENV_FILE" "$backup"
  chmod 600 "$ENV_TEMP"
  mv -f -- "$ENV_TEMP" "$ENV_FILE"
  ENV_TEMP=""
  log "Updated $ENV_FILE; previous configuration saved to $backup"
}

active_swap_names() {
  swapon --noheadings --show=NAME 2>/dev/null || true
}

persist_swap() {
  [[ -f "$FSTAB_FILE" ]] || fail "Cannot find fstab: $FSTAB_FILE"
  if awk -v swap_path="$SWAP_FILE" '
    $1 == swap_path && $3 == "swap" { found = 1 }
    END { exit(found ? 0 : 1) }
  ' "$FSTAB_FILE"; then
    log "$SWAP_FILE is already persisted in $FSTAB_FILE."
    return
  fi

  local backup
  backup="$(backup_path "$FSTAB_FILE")"
  cp -p "$FSTAB_FILE" "$backup"
  printf '\n%s none swap sw 0 0\n' "$SWAP_FILE" >> "$FSTAB_FILE"
  log "Persisted $SWAP_FILE in $FSTAB_FILE; previous file saved to $backup"
}

configure_swap() {
  (( SKIP_SWAP == 0 )) || {
    log "Swap configuration skipped by request."
    return
  }
  require_command fallocate
  require_command mkswap
  require_command swapon

  local active
  active="$(active_swap_names)"
  if grep -Fxq -- "$SWAP_FILE" <<< "$active"; then
    log "$SWAP_FILE is already active."
    persist_swap
    return
  fi
  if [[ -n "$active" ]]; then
    log "Active swap already exists; no additional swap file will be created:"
    printf '%s\n' "$active"
    return
  fi

  if [[ -e "$SWAP_FILE" ]]; then
    [[ -f "$SWAP_FILE" ]] || fail "Existing swap target is not a regular file: $SWAP_FILE"
    log "Enabling existing swap file $SWAP_FILE..."
    swapon "$SWAP_FILE" ||
      fail "$SWAP_FILE exists but is not a valid swap file; it was left unchanged."
  else
    local parent
    parent="$(dirname -- "$SWAP_FILE")"
    [[ -d "$parent" ]] || fail "Swap parent directory does not exist: $parent"
    SWAP_TEMP="$SWAP_FILE.wenzwork.$$"
    [[ ! -e "$SWAP_TEMP" ]] || fail "Temporary swap path already exists: $SWAP_TEMP"
    log "Creating $SWAP_SIZE swap file at $SWAP_FILE..."
    fallocate -l "$SWAP_SIZE" "$SWAP_TEMP" ||
      fail "Could not allocate $SWAP_SIZE for swap; check free disk space."
    chmod 600 "$SWAP_TEMP"
    mkswap "$SWAP_TEMP" >/dev/null
    mv -- "$SWAP_TEMP" "$SWAP_FILE"
    SWAP_TEMP=""
    swapon "$SWAP_FILE" ||
      fail "Could not enable $SWAP_FILE; the file was left in place for inspection."
  fi

  persist_swap
  log "Swap is active: $SWAP_FILE"
}

install_systemd() {
  (( SKIP_SYSTEMD == 0 )) || {
    log "Systemd installation skipped by request."
    return
  }
  [[ -x "$START_SCRIPT" ]] ||
    fail "start_server.sh is missing or not executable: $START_SCRIPT"
  "$START_SCRIPT" install-systemd
}

main() {
  while (( $# > 0 )); do
    case "$1" in
      --no-swap)
        SKIP_SWAP=1
        ;;
      --no-systemd)
        SKIP_SYSTEMD=1
        ;;
      -h | --help)
        usage
        return
        ;;
      *)
        usage >&2
        fail "Unknown option: $1"
        ;;
    esac
    shift
  done

  if [[ "$(uname -s)" != "Linux" ]] && [[ "${WENZWORK_ALLOW_NON_LINUX_MEMORY_CONFIG:-0}" != "1" ]]; then
    fail "This configuration script supports Linux only."
  fi
  if (( EUID != 0 )) && [[ "${WENZWORK_ALLOW_NON_ROOT_MEMORY_CONFIG:-0}" != "1" ]]; then
    fail "Run this script as root: sudo ./configure_server_memory.sh"
  fi

  validate_inputs
  report_host_memory
  configure_env
  configure_swap
  install_systemd
  log "2 GiB server memory configuration completed."
}

main "$@"
