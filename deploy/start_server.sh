#!/usr/bin/env bash

set -Eeuo pipefail
umask 027

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="${WENZWORK_BIN_DIR:-$SCRIPT_DIR/bin}"
API_BIN="$BIN_DIR/wenzwork-api"
MIGRATE_BIN="$BIN_DIR/wenzwork-migrate"
ENV_FILE="${WENZWORK_ENV_FILE:-$SCRIPT_DIR/.env}"
export WENZWORK_ENV_FILE="$ENV_FILE"
export MIGRATIONS_DIR="${MIGRATIONS_DIR:-$SCRIPT_DIR/migrations}"
PID_FILE="${WENZWORK_PID_FILE:-$SCRIPT_DIR/run/wenzwork-api.pid}"
LOG_FILE="${WENZWORK_LOG_FILE:-$SCRIPT_DIR/logs/wenzwork-api.log}"
VERSION_FILE="$SCRIPT_DIR/VERSION"
GITHUB_API_ROOT="https://api.github.com"
DEFAULT_GITHUB_REPOSITORY="lyming99/wenzwork_web"
UPGRADE_TEMP_DIR=""
GITHUB_CURL_CONFIG=""
SYSTEMD_INSTALL_TEMP=""
SYSTEMD_SERVICE_NAME="${WENZWORK_SYSTEMD_SERVICE:-wenzwork-api.service}"
SYSTEMCTL_BIN="${WENZWORK_SYSTEMCTL:-systemctl}"

log() {
  printf '[wenzwork] %s\n' "$*"
}

fail() {
  log "ERROR: $*" >&2
  exit 1
}

cleanup() {
  if [[ -n "$UPGRADE_TEMP_DIR" && -d "$UPGRADE_TEMP_DIR" ]]; then
    rm -rf -- "$UPGRADE_TEMP_DIR"
  fi
  if [[ -n "$SYSTEMD_INSTALL_TEMP" && -f "$SYSTEMD_INSTALL_TEMP" ]]; then
    rm -f -- "$SYSTEMD_INSTALL_TEMP"
  fi
}

trap cleanup EXIT

trim_whitespace() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

set_host_default() {
  local name="$1"
  local value="$2"
  if [[ ! -v $name ]] || [[ -z "${!name}" ]]; then
    printf -v "$name" '%s' "$value"
    export "$name"
  fi
}

apply_host_defaults() {
  local environment=production public_base_url=https://wenzwork.com cookie_secure=false
  if [[ "${SYSTEM_SETUP_COMPLETED:-false}" != true ]]; then
		environment=development
		public_base_url=http://localhost:8080
		cookie_secure=false
	fi
  set_host_default APP_ENV "$environment"
  set_host_default PUBLIC_BASE_URL "$public_base_url"
  set_host_default HTTP_ADDR :8080
  set_host_default WEB_ROOT "$SCRIPT_DIR/web"
  set_host_default LOG_LEVEL info
  set_host_default REGISTRATION_ENABLED true
  set_host_default COOKIE_SECURE "$cookie_secure"
  set_host_default ADMIN_MFA_REQUIRED false
  set_host_default ALLOWED_ORIGINS "$PUBLIC_BASE_URL"
  set_host_default HOST_SECRETS_FILE "$SCRIPT_DIR/cache/host-secrets/application.env"
  set_host_default RELEASE_ASSET_CACHE_DIR "$SCRIPT_DIR/cache/releases"
  set_host_default GITHUB_RELEASE_REPOSITORY "$DEFAULT_GITHUB_REPOSITORY"
  set_host_default RELAY_DEVELOPMENT_CA_DIR "$SCRIPT_DIR/cache/host-secrets/relay-ca"
  set_host_default RELAY_BOOTSTRAP_ASSETS_DIR "$SCRIPT_DIR/relay-bootstrap"
  set_host_default REMOTE_MVP_ENABLED true
}

load_env() {
  if [[ ! -f "$ENV_FILE" ]]; then
    if [[ -n "${DATABASE_URL:-}" ]]; then
      apply_host_defaults
      return
    fi
    if [[ -f "$SCRIPT_DIR/.env.example" ]]; then
      cp "$SCRIPT_DIR/.env.example" "$ENV_FILE"
      chmod 600 "$ENV_FILE"
      fail "Created $ENV_FILE. Set SYSTEM_ADMIN_EMAIL and SYSTEM_ADMIN_PASSWORD, then run this command again."
    fi
    fail "Missing $ENV_FILE and DATABASE_URL is not set."
  fi

  local line key value
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    [[ "$line" =~ ^[[:space:]]*$ || "$line" =~ ^[[:space:]]*# ]] && continue
    line="${line#export }"
    if [[ ! "$line" =~ ^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*=(.*)$ ]]; then
      fail "Invalid environment entry in $ENV_FILE: $line"
    fi

    key="${BASH_REMATCH[1]}"
    value="$(decode_env_value "${BASH_REMATCH[2]}")"

    if [[ ! -v $key ]]; then
      export "$key=$value"
    fi
  done < "$ENV_FILE"
  apply_host_defaults
}

decode_env_value() {
	local value first last
	value="$(trim_whitespace "$1")"
	if (( ${#value} >= 2 )); then
		first="${value:0:1}"
		last="${value: -1}"
		if [[ "$first" == '"' && "$last" == '"' ]]; then
			value="${value:1:${#value}-2}"
			value="$(decode_double_quoted_value "$value")"
		elif [[ "$first" == "'" && "$last" == "'" ]]; then
			value="${value:1:${#value}-2}"
		fi
	fi
	printf '%s' "$value"
}

decode_double_quoted_value() {
	local value="$1" output='' character next index length
	length="${#value}"
	for ((index = 0; index < length; index++)); do
		character="${value:index:1}"
		if [[ "$character" != '\' ]]; then
			output+="$character"
			continue
		fi
		index=$((index + 1))
		(( index < length )) || fail "Invalid trailing escape in environment value."
		next="${value:index:1}"
		case "$next" in
			'\' | '"') output+="$next" ;;
			*) output+="\\$next" ;;
		esac
	done
	printf '%s' "$output"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "Required command not found: $1"
}

require_binaries() {
  [[ -x "$API_BIN" ]] || fail "API binary is missing or not executable: $API_BIN"
  [[ -x "$MIGRATE_BIN" ]] || fail "Migration binary is missing or not executable: $MIGRATE_BIN"
  [[ -d "$SCRIPT_DIR/migrations" ]] || fail "Migration directory is missing: $SCRIPT_DIR/migrations"
}

validate_systemd_service_name() {
  [[ "$SYSTEMD_SERVICE_NAME" =~ ^[A-Za-z0-9_.@-]+\.service$ ]] ||
    fail "WENZWORK_SYSTEMD_SERVICE must be a simple .service unit name."
}

systemd_service_installed() {
  validate_systemd_service_name
  command -v "$SYSTEMCTL_BIN" >/dev/null 2>&1 || return 1
  local load_state
  load_state="$("$SYSTEMCTL_BIN" show --property=LoadState --value "$SYSTEMD_SERVICE_NAME" 2>/dev/null || true)"
  [[ "$load_state" == "loaded" ]]
}

process_manager() {
  local requested="${WENZWORK_PROCESS_MANAGER:-auto}"
  case "$requested" in
    auto)
      if systemd_service_installed; then
        printf 'systemd\n'
      else
        printf 'standalone\n'
      fi
      ;;
    systemd)
      systemd_service_installed ||
        fail "The systemd unit $SYSTEMD_SERVICE_NAME is not installed. Run: sudo ./start_server.sh install-systemd"
      printf 'systemd\n'
      ;;
    standalone)
      printf 'standalone\n'
      ;;
    *)
      fail "WENZWORK_PROCESS_MANAGER must be auto, systemd, or standalone."
      ;;
  esac
}

systemd_is_active() {
  "$SYSTEMCTL_BIN" is-active --quiet "$SYSTEMD_SERVICE_NAME" 2>/dev/null
}

systemd_main_pid() {
  "$SYSTEMCTL_BIN" show --property=MainPID --value "$SYSTEMD_SERVICE_NAME" 2>/dev/null || printf '0\n'
}

host_memory_summary() {
  [[ -r /proc/meminfo ]] || return
  local mem_total_kib mem_available_kib swap_total_kib
  read -r mem_total_kib mem_available_kib swap_total_kib < <(
    awk '
      /^MemTotal:/ { total = $2 }
      /^MemAvailable:/ { available = $2 }
      /^MemFree:/ { free = $2 }
      /^Buffers:/ { buffers = $2 }
      /^Cached:/ { cached = $2 }
      /^SReclaimable:/ { reclaimable = $2 }
      /^Shmem:/ { shared = $2 }
      /^SwapTotal:/ { swap = $2 }
      END {
        if (available <= 0) {
          available = free + buffers + cached + reclaimable - shared
        }
        if (available < 0) {
          available = 0
        }
        printf "%d %d %d\n", total, available, swap
      }
    ' /proc/meminfo
  )
  [[ "$mem_total_kib" =~ ^[0-9]+$ && "$mem_available_kib" =~ ^[0-9]+$ && "$swap_total_kib" =~ ^[0-9]+$ ]] ||
    return

  log "Host memory: total $((mem_total_kib / 1024)) MiB, available $((mem_available_kib / 1024)) MiB, swap $((swap_total_kib / 1024)) MiB."
  if (( swap_total_kib == 0 && mem_available_kib < 262144 )); then
    log "WARNING: less than 256 MiB is available and swap is disabled; the kernel may OOM-kill the API."
  fi
  if [[ -n "${GOMEMLIMIT:-}" ]]; then
    log "Go runtime memory soft limit: $GOMEMLIMIT"
  fi
}

read_pid() {
  [[ -f "$PID_FILE" ]] || return 1
  local pid
  IFS= read -r pid < "$PID_FILE"
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  printf '%s\n' "$pid"
}

standalone_is_running() {
  local pid command_line
  pid="$(read_pid)" || return 1
  kill -0 "$pid" 2>/dev/null || return 1

  if [[ -r "/proc/$pid/cmdline" ]]; then
    command_line="$(tr '\0' ' ' < "/proc/$pid/cmdline")"
    [[ "$command_line" == *"$API_BIN"* ]] || return 1
  fi
}

verify_owned_process() {
  local pid="$1"
  if [[ -r "/proc/$pid/cmdline" ]]; then
    local command_line
    command_line="$(tr '\0' ' ' < "/proc/$pid/cmdline")"
    [[ "$command_line" == *"$API_BIN"* ]] ||
      fail "PID $pid does not belong to $API_BIN. Remove $PID_FILE only after checking the process."
  fi
}

run_migrations() {
  [[ -n "${DATABASE_URL:-}" ]] || fail "DATABASE_URL is required."
  log "Applying database migrations..."
  (
    cd "$SCRIPT_DIR"
    unset GITHUB_ACCESS_TOKEN
    "$MIGRATE_BIN" up
  )
}

healthcheck_url() {
  if [[ -n "${WENZWORK_HEALTHCHECK_URL:-}" ]]; then
    printf '%s\n' "$WENZWORK_HEALTHCHECK_URL"
    return
  fi

  local address="${HTTP_ADDR:-:8080}"
  local port="${address##*:}"
  [[ "$port" =~ ^[0-9]+$ ]] || port=8080
  printf 'http://127.0.0.1:%s/api/v1/health/ready\n' "$port"
}

start_standalone_server() {
  load_env
  unset GITHUB_ACCESS_TOKEN
  require_command nohup
  require_binaries

  if standalone_is_running; then
    log "Server is already running (PID $(read_pid))."
    return
  fi
  rm -f "$PID_FILE"

  run_migrations
  host_memory_summary
  mkdir -p "$(dirname -- "$PID_FILE")" "$(dirname -- "$LOG_FILE")"

  log "Starting WenzWork server..."
  (
    cd "$SCRIPT_DIR"
    # The GitHub upgrade token is only needed by the deployment scripts and
    # should not be inherited by the long-running API process.
    unset GITHUB_ACCESS_TOKEN
    nohup "$API_BIN" >> "$LOG_FILE" 2>&1 </dev/null &
    printf '%s\n' "$!" > "$PID_FILE"
  )

  local pid timeout url attempt
  pid="$(read_pid)" || fail "The server started without writing a valid PID."
  timeout="${WENZWORK_STARTUP_TIMEOUT:-30}"
  [[ "$timeout" =~ ^[0-9]+$ && "$timeout" -gt 0 ]] || fail "WENZWORK_STARTUP_TIMEOUT must be a positive integer."
  url="$(healthcheck_url)"

  if [[ "$url" == "off" ]] || ! command -v curl >/dev/null 2>&1; then
    sleep 1
    kill -0 "$pid" 2>/dev/null || fail "Server exited during startup. Check $LOG_FILE"
    log "Server is running (PID $pid). Log: $LOG_FILE"
    return
  fi

  for ((attempt = 1; attempt <= timeout; attempt++)); do
    if ! kill -0 "$pid" 2>/dev/null; then
      rm -f "$PID_FILE"
      fail "Server exited during startup. Check $LOG_FILE"
    fi
    if curl --fail --progress-bar --show-error --max-time 2 "$url" >/dev/null 2>&1; then
      log "Server is ready (PID $pid): $url"
      return
    fi
    sleep 1
  done

  fail "Server is running but did not become ready within ${timeout}s. Check $LOG_FILE"
}

stop_standalone_server() {
  local pid timeout attempt
  if ! standalone_is_running; then
    rm -f "$PID_FILE"
    log "Server is not running."
    return
  fi

  pid="$(read_pid)"
  verify_owned_process "$pid"
  timeout="${WENZWORK_STOP_TIMEOUT:-15}"
  [[ "$timeout" =~ ^[0-9]+$ && "$timeout" -gt 0 ]] || fail "WENZWORK_STOP_TIMEOUT must be a positive integer."

  log "Stopping WenzWork server (PID $pid)..."
  kill -TERM "$pid"
  for ((attempt = 1; attempt <= timeout; attempt++)); do
    if ! kill -0 "$pid" 2>/dev/null; then
      rm -f "$PID_FILE"
      log "Server stopped."
      return
    fi
    sleep 1
  done

  log "Graceful shutdown timed out; forcing PID $pid to stop."
  kill -KILL "$pid" 2>/dev/null || true
  rm -f "$PID_FILE"
}

status_standalone_server() {
  if standalone_is_running; then
    log "Server is running (PID $(read_pid)). Log: $LOG_FILE"
  else
    rm -f "$PID_FILE"
    log "Server is not running."
    return 1
  fi
}

wait_for_systemd_ready() {
  local timeout url attempt pid
  timeout="${WENZWORK_STARTUP_TIMEOUT:-30}"
  [[ "$timeout" =~ ^[0-9]+$ && "$timeout" -gt 0 ]] ||
    fail "WENZWORK_STARTUP_TIMEOUT must be a positive integer."
  url="$(healthcheck_url)"

  if [[ "$url" == "off" ]] || ! command -v curl >/dev/null 2>&1; then
    sleep 1
    systemd_is_active ||
      fail "$SYSTEMD_SERVICE_NAME did not stay active. Check: journalctl -u $SYSTEMD_SERVICE_NAME"
    pid="$(systemd_main_pid)"
    log "Server is managed by systemd and is active (PID ${pid:-0})."
    return
  fi

  for ((attempt = 1; attempt <= timeout; attempt++)); do
    systemd_is_active ||
      fail "$SYSTEMD_SERVICE_NAME stopped during startup. Check: journalctl -u $SYSTEMD_SERVICE_NAME"
    if curl --fail --progress-bar --show-error --max-time 2 "$url" >/dev/null 2>&1; then
      pid="$(systemd_main_pid)"
      log "Server is ready under systemd (PID ${pid:-0}): $url"
      return
    fi
    sleep 1
  done

  fail "$SYSTEMD_SERVICE_NAME did not become ready within ${timeout}s. Check: journalctl -u $SYSTEMD_SERVICE_NAME"
}

start_systemd_server() {
  load_env
  unset GITHUB_ACCESS_TOKEN
  require_binaries
  host_memory_summary
  log "Starting $SYSTEMD_SERVICE_NAME..."
  if ! "$SYSTEMCTL_BIN" start "$SYSTEMD_SERVICE_NAME"; then
    fail "Could not start $SYSTEMD_SERVICE_NAME. Run this command with systemd management permission."
  fi
  wait_for_systemd_ready
}

stop_systemd_server() {
  if ! systemd_is_active; then
    log "$SYSTEMD_SERVICE_NAME is not running."
    return
  fi
  log "Stopping $SYSTEMD_SERVICE_NAME..."
  if ! "$SYSTEMCTL_BIN" stop "$SYSTEMD_SERVICE_NAME"; then
    fail "Could not stop $SYSTEMD_SERVICE_NAME. Run this command with systemd management permission."
  fi
  log "Server stopped."
}

restart_systemd_server() {
  load_env
  unset GITHUB_ACCESS_TOKEN
  require_binaries
  host_memory_summary
  log "Restarting $SYSTEMD_SERVICE_NAME..."
  if ! "$SYSTEMCTL_BIN" restart "$SYSTEMD_SERVICE_NAME"; then
    fail "Could not restart $SYSTEMD_SERVICE_NAME. Run this command with systemd management permission."
  fi
  wait_for_systemd_ready
}

status_systemd_server() {
  local state pid restarts
  state="$("$SYSTEMCTL_BIN" is-active "$SYSTEMD_SERVICE_NAME" 2>/dev/null || true)"
  if [[ "$state" != "active" ]]; then
    log "$SYSTEMD_SERVICE_NAME is ${state:-unavailable}."
    return 1
  fi
  pid="$(systemd_main_pid)"
  restarts="$("$SYSTEMCTL_BIN" show --property=NRestarts --value "$SYSTEMD_SERVICE_NAME" 2>/dev/null || printf 'unknown\n')"
  log "$SYSTEMD_SERVICE_NAME is active (PID ${pid:-0}, automatic restarts ${restarts:-unknown})."
}

start_server() {
  local manager
  manager="$(process_manager)" || return 1
  if [[ "$manager" == "systemd" ]]; then
    start_systemd_server
  else
    start_standalone_server
  fi
}

stop_server() {
  local manager
  manager="$(process_manager)" || return 1
  if [[ "$manager" == "systemd" ]]; then
    stop_systemd_server
  else
    stop_standalone_server
  fi
}

restart_server() {
  local manager
  manager="$(process_manager)" || return 1
  if [[ "$manager" == "systemd" ]]; then
    restart_systemd_server
  else
    stop_standalone_server
    start_standalone_server
  fi
}

status_server() {
  local manager
  manager="$(process_manager)" || return 1
  if [[ "$manager" == "systemd" ]]; then
    status_systemd_server
  else
    status_standalone_server
  fi
}

migrate_only() {
  load_env
  unset GITHUB_ACCESS_TOKEN
  require_binaries
  run_migrations
}

run_foreground() {
  load_env
  unset GITHUB_ACCESS_TOKEN
  require_binaries
  host_memory_summary
  cd "$SCRIPT_DIR"
  exec "$API_BIN"
}

systemd_quote() {
  local value="$1"
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] ||
    fail "Systemd paths must not contain line breaks."
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//%/%%}"
  printf '"%s"' "$value"
}

validate_systemd_memory_value() {
  local name="$1"
  local value="$2"
  [[ -z "$value" || "$value" =~ ^[0-9]+([KMGTPE]i?B?)?$ || "$value" =~ ^[0-9]+%$ ]] ||
    fail "$name must be a systemd byte size such as 320M, 1G, or a percentage."
}

install_systemd_service() {
  if [[ "$(uname -s)" != "Linux" ]] && [[ "${WENZWORK_ALLOW_NON_LINUX_SYSTEMD_INSTALL:-0}" != "1" ]]; then
    fail "Systemd installation is supported only on Linux."
  fi
  if (( EUID != 0 )) && [[ "${WENZWORK_ALLOW_NON_ROOT_SYSTEMD_INSTALL:-0}" != "1" ]]; then
    fail "Run systemd installation as root: sudo ./start_server.sh install-systemd"
  fi

  load_env
  unset GITHUB_ACCESS_TOKEN
  require_binaries
  require_command install
  require_command mktemp
  require_command stat
  require_command "$SYSTEMCTL_BIN"
  validate_systemd_service_name

  local unit_dir unit_path service_user service_group directory_owner directory_group
  local quoted_script_dir quoted_start_script memory_high memory_max
  unit_dir="${WENZWORK_SYSTEMD_UNIT_DIR:-/etc/systemd/system}"
  [[ "$unit_dir" == /* && "$unit_dir" != "/" ]] ||
    fail "WENZWORK_SYSTEMD_UNIT_DIR must be an absolute directory other than /."
  mkdir -p "$unit_dir"
  unit_path="$unit_dir/$SYSTEMD_SERVICE_NAME"

  directory_owner="$(stat -c '%U' "$SCRIPT_DIR")"
  directory_group="$(stat -c '%G' "$SCRIPT_DIR")"
  service_user="${WENZWORK_SERVICE_USER:-$directory_owner}"
  id "$service_user" >/dev/null 2>&1 ||
    fail "WENZWORK_SERVICE_USER does not identify a local user: $service_user"
  service_group="${WENZWORK_SERVICE_GROUP:-$directory_group}"
  [[ "$service_user" != *[[:space:]]* && "$service_group" != *[[:space:]]* ]] ||
    fail "Systemd service user and group names must not contain whitespace."
  if [[ "$service_user" == "root" || "$(id -u "$service_user")" == "0" ]]; then
    log "WARNING: the API will run as root. Set WENZWORK_SERVICE_USER to a dedicated deployment user when possible."
  fi

  memory_high="${WENZWORK_MEMORY_HIGH:-}"
  memory_max="${WENZWORK_MEMORY_MAX:-}"
  validate_systemd_memory_value WENZWORK_MEMORY_HIGH "$memory_high"
  validate_systemd_memory_value WENZWORK_MEMORY_MAX "$memory_max"

  if systemd_service_installed; then
    stop_systemd_server
  fi
  # A stale/manual nohup process must not keep port 8080 occupied when the
  # systemd unit is installed for the first time.
  stop_standalone_server

  quoted_script_dir="$(systemd_quote "$SCRIPT_DIR")"
  quoted_start_script="$(systemd_quote "$SCRIPT_DIR/start_server.sh")"
  SYSTEMD_INSTALL_TEMP="$(mktemp "${TMPDIR:-/tmp}/wenzwork-systemd.XXXXXX")"
  {
    cat <<EOF
[Unit]
Description=WenzWork API
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=$service_user
Group=$service_group
WorkingDirectory=$quoted_script_dir
ExecStartPre=$quoted_start_script migrate
ExecStart=$quoted_start_script run
Restart=always
RestartSec=3s
KillSignal=SIGTERM
TimeoutStartSec=60s
TimeoutStopSec=20s
UMask=0027
LimitNOFILE=65535
NoNewPrivileges=true
PrivateTmp=true
MemoryAccounting=true
TasksAccounting=true
Environment=GOTRACEBACK=all
EOF
    [[ -z "$memory_high" ]] || printf 'MemoryHigh=%s\n' "$memory_high"
    [[ -z "$memory_max" ]] || printf 'MemoryMax=%s\n' "$memory_max"
    cat <<'EOF'
StandardOutput=journal
StandardError=journal
SyslogIdentifier=wenzwork-api

[Install]
WantedBy=multi-user.target
EOF
  } > "$SYSTEMD_INSTALL_TEMP"

  install -m 0644 "$SYSTEMD_INSTALL_TEMP" "$unit_path"
  rm -f -- "$SYSTEMD_INSTALL_TEMP"
  SYSTEMD_INSTALL_TEMP=""
  rm -f "$PID_FILE"

  "$SYSTEMCTL_BIN" daemon-reload ||
    fail "systemctl daemon-reload failed."
  "$SYSTEMCTL_BIN" enable "$SYSTEMD_SERVICE_NAME" ||
    fail "Could not enable $SYSTEMD_SERVICE_NAME."
  start_systemd_server
  log "Installed $unit_path. Future starts and upgrades will use systemd automatically."
}

download_file() {
  local url="$1"
  local destination="$2"
  curl --fail --location --progress-bar --show-error --retry 3 --retry-delay 2 \
    --connect-timeout 10 --max-time 1800 --output "$destination" "$url"
}

prepare_github_auth() {
  local token="$1"
  GITHUB_CURL_CONFIG=""
  [[ -n "$token" ]] || return
  [[ "$token" =~ ^[A-Za-z0-9_.-]+$ ]] ||
    fail "GITHUB_ACCESS_TOKEN contains unsupported characters."

  GITHUB_CURL_CONFIG="$UPGRADE_TEMP_DIR/github-curl.conf"
  (umask 077; printf 'header = "Authorization: Bearer %s"\n' "$token" > "$GITHUB_CURL_CONFIG")
  chmod 600 "$GITHUB_CURL_CONFIG"
}

github_api_download() {
  local url="$1"
  local destination="$2"
  local accept="$3"
  local description="$4"
  local auth_args=()
  if [[ -n "$GITHUB_CURL_CONFIG" ]]; then
    auth_args=(--config "$GITHUB_CURL_CONFIG")
  fi

  if ! curl "${auth_args[@]}" --fail --location --progress-bar --show-error \
    --retry 3 --retry-delay 2 --connect-timeout 10 --max-time 1800 \
    --proto '=https' --proto-redir '=https' \
    --header "Accept: $accept" \
    --header 'X-GitHub-Api-Version: 2022-11-28' \
    --header 'User-Agent: wenzwork-server-upgrader' \
    --output "$destination" "$url"; then
    fail "Could not download $description from GitHub. Check GITHUB_RELEASE_REPOSITORY, GITHUB_ACCESS_TOKEN Contents: read permission, and server outbound access."
  fi
}

json_string_tokens() {
  # GitHub does not guarantee that JSON fields are separated by newlines. Emit
  # every JSON string token so the callers work with both pretty-printed and
  # compact responses without introducing a jq/python runtime dependency.
  awk '
    BEGIN {
      in_string = 0
      escaped = 0
      token = ""
    }
    {
      for (index_in_line = 1; index_in_line <= length($0); index_in_line++) {
        character = substr($0, index_in_line, 1)
        if (!in_string) {
          if (character == "\"") {
            in_string = 1
            escaped = 0
            token = ""
          }
          continue
        }

        if (escaped) {
          if (character == "\"" || character == "\\" || character == "/") {
            token = token character
          } else {
            token = token "\\" character
          }
          escaped = 0
        } else if (character == "\\") {
          escaped = 1
        } else if (character == "\"") {
          print token
          token = ""
          in_string = 0
        } else {
          token = token character
        }
      }
    }
    END {
      if (in_string || escaped) {
        exit 2
      }
    }
  ' "$1"
}

release_tag_from_metadata() {
  json_string_tokens "$1" | awk '
    found { next }
    expect_value {
      print
      found = 1
      next
    }
    $0 == "tag_name" { expect_value = 1 }
  '
}

release_asset_api_url() {
  local metadata="$1"
  local asset_name="$2"
  local repository="$3"
  local url prefix asset_id
  prefix="$GITHUB_API_ROOT/repos/$repository/releases/assets/"
  url="$(json_string_tokens "$metadata" | awk -v target="$asset_name" -v required_prefix="$prefix" '
    found { next }
    expect_value == "url" {
      asset_url = $0
      expect_value = ""
      next
    }
    expect_value == "name" {
      if ($0 == target && index(asset_url, required_prefix) == 1) {
        print asset_url
        found = 1
      }
      expect_value = ""
      next
    }
    $0 == "url" { expect_value = "url"; next }
    $0 == "name" { expect_value = "name"; next }
  ')"

  [[ "$url" == "$prefix"* ]] || fail "GitHub Release is missing asset: $asset_name"
  asset_id="${url#"$prefix"}"
  [[ "$asset_id" =~ ^[0-9]+$ ]] || fail "GitHub returned an invalid asset reference for $asset_name"
  printf '%s\n' "$url"
}

verify_checksum() {
  local archive="$1"
  local checksum_file="$2"
  local archive_name="$3"
  local expected actual

  expected="$(awk -v name="$archive_name" '
    {
      path = $2
      # Checksums produced on Windows may use CRLF line endings. awk removes
      # the LF record separator but leaves the trailing CR on the file name.
      sub(/\r$/, "", path)
      sub(/^\.\//, "", path)
      count = split(path, parts, "/")
      if (parts[count] == name) { print $1; exit }
    }
  ' "$checksum_file")"
  [[ "$expected" =~ ^[0-9a-fA-F]{64}$ ]] || fail "No checksum found for $archive_name"

  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$archive" | awk '{print $1}')"
    # GNU coreutils under Git Bash prefixes escaped Windows paths with a
    # backslash; the digest itself remains the following 64 hexadecimal bytes.
    actual="${actual#\\}"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$archive" | awk '{print $1}')"
  else
    fail "sha256sum or shasum is required to verify upgrades."
  fi

  [[ "${actual,,}" == "${expected,,}" ]] || fail "SHA-256 verification failed for $archive_name"
  log "SHA-256 verified: $archive_name"
}

validate_archive_paths() {
  local archive="$1"
  local entry
  while IFS= read -r entry; do
    case "$entry" in
      /* | ../* | */../* | */..)
        fail "Unsafe path in upgrade archive: $entry"
        ;;
    esac
  done < <(tar -tzf "$archive")
}

upgrade_server() {
  load_env
  local github_access_token="${GITHUB_ACCESS_TOKEN:-}"
  unset GITHUB_ACCESS_TOKEN
  require_command tar
  require_command mktemp

  local source="${1:-}"
  local supplied_checksum="${2:-}"
  local repository metadata tag safe_tag archive_name checksum_name
  local archive_url checksum_url archive checksum_file stage_dir next_version current_version backup_dir

  UPGRADE_TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/wenzwork-upgrade.XXXXXX")"
  archive="$UPGRADE_TEMP_DIR/server.tar.gz"
  checksum_file="$UPGRADE_TEMP_DIR/SHA256SUMS.txt"

  if [[ -z "$source" ]]; then
    require_command curl
    repository="${GITHUB_RELEASE_REPOSITORY:-$DEFAULT_GITHUB_REPOSITORY}"
    [[ "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] ||
      fail "GITHUB_RELEASE_REPOSITORY must use owner/repository format."
    prepare_github_auth "$github_access_token"
    github_access_token=""
    metadata="$UPGRADE_TEMP_DIR/latest-release.json"
    github_api_download \
      "$GITHUB_API_ROOT/repos/$repository/releases/latest" \
      "$metadata" 'application/vnd.github+json' 'latest Release metadata'
    tag="$(release_tag_from_metadata "$metadata")"
    [[ "$tag" =~ ^[A-Za-z0-9._-]+$ ]] || fail "GitHub returned an unsupported release tag."
    safe_tag="$(printf '%s' "$tag" | sed 's/[^A-Za-z0-9._-]/-/g')"
    archive_name="wenzwork-server-linux-amd64-$safe_tag.tar.gz"
    checksum_name="wenzwork-$safe_tag-SHA256SUMS.txt"

    current_version="$(cat "$VERSION_FILE" 2>/dev/null || true)"
    if [[ "$current_version" == "$tag" && "${WENZWORK_FORCE_UPGRADE:-0}" != "1" ]]; then
      log "Already running the latest packaged version: $tag"
      return
    fi

    archive_url="$(release_asset_api_url "$metadata" "$archive_name" "$repository")"
    checksum_url="$(release_asset_api_url "$metadata" "$checksum_name" "$repository")"
    log "Downloading $archive_name..."
    github_api_download "$archive_url" "$archive" 'application/octet-stream' "$archive_name"
    github_api_download "$checksum_url" "$checksum_file" 'application/octet-stream' "$checksum_name"
    verify_checksum "$archive" "$checksum_file" "$archive_name"
  else
    if [[ "$source" =~ ^https?:// ]]; then
      require_command curl
      archive_name="${source%%\?*}"
      archive_name="${archive_name##*/}"
      download_file "$source" "$archive"
    else
      [[ -f "$source" ]] || fail "Upgrade package not found: $source"
      archive_name="$(basename -- "$source")"
      cp "$source" "$archive"
    fi

    if [[ -n "$supplied_checksum" ]]; then
      if [[ "$supplied_checksum" =~ ^https?:// ]]; then
        require_command curl
        download_file "$supplied_checksum" "$checksum_file"
      else
        [[ -f "$supplied_checksum" ]] || fail "Checksum file not found: $supplied_checksum"
        cp "$supplied_checksum" "$checksum_file"
      fi
      verify_checksum "$archive" "$checksum_file" "$archive_name"
    else
      log "WARNING: no checksum file supplied for the local upgrade package."
    fi
  fi

  validate_archive_paths "$archive"
  stage_dir="$UPGRADE_TEMP_DIR/stage"
  mkdir -p "$stage_dir"
  tar -xzf "$archive" -C "$stage_dir"
  # Secure hosts commonly mount /tmp with noexec. Checking -x inside the
  # staging directory would reject valid binaries on those filesystems, so
  # validate their contents here and normalize permissions after installation.
  [[ -f "$stage_dir/bin/wenzwork-api" && -s "$stage_dir/bin/wenzwork-api" ]] ||
    fail "Upgrade package is missing or has an empty bin/wenzwork-api"
  [[ -f "$stage_dir/bin/wenzwork-migrate" && -s "$stage_dir/bin/wenzwork-migrate" ]] ||
    fail "Upgrade package is missing or has an empty bin/wenzwork-migrate"
  [[ -f "$stage_dir/init_server.sh" ]] || fail "Upgrade package is missing init_server.sh"
  [[ -f "$stage_dir/start_server.sh" ]] || fail "Upgrade package is missing start_server.sh"
  [[ -f "$stage_dir/stop_server.sh" ]] || fail "Upgrade package is missing stop_server.sh"
  [[ -f "$stage_dir/upgrade_server.sh" ]] || fail "Upgrade package is missing upgrade_server.sh"
  [[ -f "$stage_dir/upgrage_server.sh" ]] || fail "Upgrade package is missing upgrage_server.sh"
  [[ -d "$stage_dir/migrations" ]] || fail "Upgrade package is missing migrations"
  [[ -f "$stage_dir/web/index.html" ]] || fail "Upgrade package is missing web/index.html"
  [[ -f "$stage_dir/VERSION" ]] || fail "Upgrade package is missing VERSION"

  next_version="$(cat "$stage_dir/VERSION")"
  [[ "$next_version" =~ ^[A-Za-z0-9._-]+$ ]] || fail "Upgrade package contains an invalid VERSION"
  if [[ -z "$source" && "$next_version" != "$tag" ]]; then
    fail "Upgrade package VERSION $next_version does not match GitHub Release $tag"
  fi
  current_version="$(cat "$VERSION_FILE" 2>/dev/null || printf 'unknown')"
  if [[ "$next_version" == "$current_version" && "${WENZWORK_FORCE_UPGRADE:-0}" != "1" ]]; then
    log "Package version $next_version is already installed."
    return
  fi

  backup_dir="$SCRIPT_DIR/backups/$(date -u +%Y%m%dT%H%M%SZ)"
  mkdir -p "$backup_dir"
  local item
  for item in bin migrations web docs relay-bootstrap init_server.sh start_server.sh stop_server.sh upgrade_server.sh upgrage_server.sh configure_server_memory.sh Caddyfile .env.example VERSION; do
    [[ -e "$SCRIPT_DIR/$item" ]] && cp -a "$SCRIPT_DIR/$item" "$backup_dir/"
  done
  log "Current files backed up to $backup_dir"

  stop_server
  # Replace the complete web build so obsolete hashed assets cannot accumulate.
  [[ "$SCRIPT_DIR/web" != "/" && "$SCRIPT_DIR/web" != "$SCRIPT_DIR" ]] ||
    fail "Refusing to replace an unsafe web directory path."
  rm -rf -- "$SCRIPT_DIR/web"
  cp -a "$stage_dir"/. "$SCRIPT_DIR"/
  chmod 0755 \
    "$SCRIPT_DIR/init_server.sh" "$SCRIPT_DIR/start_server.sh" \
    "$SCRIPT_DIR/stop_server.sh" "$SCRIPT_DIR/upgrade_server.sh" "$SCRIPT_DIR/upgrage_server.sh" \
    "$API_BIN" "$MIGRATE_BIN"
  [[ ! -f "$SCRIPT_DIR/configure_server_memory.sh" ]] ||
    chmod 0755 "$SCRIPT_DIR/configure_server_memory.sh"
  [[ ! -f "$BIN_DIR/wenzwork-admin" ]] || chmod 0755 "$BIN_DIR/wenzwork-admin"

  log "Installed package $next_version. Running migrations and starting the server..."
  if ! start_server; then
    log "Upgrade did not start cleanly. The previous files remain in $backup_dir"
    return 1
  fi
  log "Upgrade complete: $current_version -> $next_version"
}

usage() {
  cat <<'EOF'
Usage: ./start_server.sh [command] [arguments]

Commands:
  start                 Start WenzWork with systemd when installed (default)
  stop                  Gracefully stop WenzWork
  restart               Restart WenzWork and verify readiness
  status                Show the WenzWork process status
  install-systemd       Install, enable, and start the production systemd unit
  migrate               Apply database migrations without starting the API
  run                   Run the API in the foreground (used by systemd)
  upgrade               Download, verify, and install the latest GitHub Release
  upgrade PACKAGE [SUM] Install a local/remote package with an optional checksum file
  help                   Show this help

The standalone stop_server.sh and upgrade_server.sh entrypoints call the same
process-control and upgrade implementation. Configure GITHUB_ACCESS_TOKEN in
.env when the Release repository is private. After install-systemd, lifecycle
commands and upgrades automatically use the installed unit. Set
WENZWORK_PROCESS_MANAGER=standalone only for recovery or non-systemd hosts.
EOF
}

main() {
  local command="${1:-start}"
  case "$command" in
    start)
      [[ -x "$SCRIPT_DIR/init_server.sh" ]] || fail "Initialization script is missing: $SCRIPT_DIR/init_server.sh"
      "$SCRIPT_DIR/init_server.sh"
      start_server
      ;;
    stop)
      stop_server
      ;;
    restart)
      restart_server
      ;;
    status)
      status_server
      ;;
    install-systemd)
      install_systemd_service
      ;;
    migrate)
      migrate_only
      ;;
    run)
      run_foreground
      ;;
    upgrade)
      upgrade_server "${2:-}" "${3:-}"
      ;;
    help | -h | --help)
      usage
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
}

main "$@"
