#!/usr/bin/env bash

set -Eeuo pipefail
umask 077

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="${WENZWORK_BIN_DIR:-$SCRIPT_DIR/bin}"
ENV_FILE="${WENZWORK_ENV_FILE:-$SCRIPT_DIR/.env}"
export WENZWORK_ENV_FILE="$ENV_FILE"
export MIGRATIONS_DIR="${MIGRATIONS_DIR:-$SCRIPT_DIR/migrations}"
ADMIN_BIN="$BIN_DIR/wenzwork-admin"
MIGRATE_BIN="$BIN_DIR/wenzwork-migrate"
TEMP_FILES=()
ADMIN_ALREADY_INITIALIZED=0
ENV_FILE_CREATED=0

log() {
  printf '[wenzwork-init] %s\n' "$*"
}

fail() {
  log "ERROR: $*" >&2
  exit 1
}

cleanup() {
  local path
  for path in "${TEMP_FILES[@]}"; do
    [[ -f "$path" ]] && rm -f -- "$path"
  done
  return 0
}

trap cleanup EXIT

trim_whitespace() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

parse_env_line() {
  local line
  line="$(trim_whitespace "$1")"
  if [[ "$line" == export[[:space:]]* ]]; then
    line="${line#export}"
    line="$(trim_whitespace "$line")"
  fi
  printf '%s' "$line"
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
		if [[ "$character" != "\\" ]]; then
			output+="$character"
			continue
		fi
		index=$((index + 1))
		(( index < length )) || fail "Invalid trailing escape in environment value."
		next="${value:index:1}"
		case "$next" in
			"\\" | "\"") output+="$next" ;;
			*) output+="\\$next" ;;
		esac
	done
	printf '%s' "$output"
}

read_env_value() {
  local wanted="$1"
  local line parsed key value
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    parsed="$(parse_env_line "$line")"
    [[ -z "$parsed" || "$parsed" == \#* ]] && continue
    if [[ "$parsed" =~ ^([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*=(.*)$ ]]; then
      key="${BASH_REMATCH[1]}"
      value="${BASH_REMATCH[2]}"
      if [[ "$key" == "$wanted" ]]; then
        decode_env_value "$value"
        return 0
      fi
    fi
  done < "$ENV_FILE"
  return 1
}

load_env() {
  local line parsed key value
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    parsed="$(parse_env_line "$line")"
    [[ -z "$parsed" || "$parsed" == \#* ]] && continue
    if [[ ! "$parsed" =~ ^([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*=(.*)$ ]]; then
      fail "Invalid environment entry in $ENV_FILE: $line"
    fi
    key="${BASH_REMATCH[1]}"
    value="$(decode_env_value "${BASH_REMATCH[2]}")"
    export "$key=$value"
  done < "$ENV_FILE"
}

set_env_value() {
  local wanted="$1"
  local replacement="$2"
  local temporary line parsed found=0
  temporary="$(mktemp "${ENV_FILE}.tmp.XXXXXX")"
  TEMP_FILES+=("$temporary")

  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    parsed="$(parse_env_line "$line")"
    if [[ "$parsed" =~ ^([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*= ]] && [[ "${BASH_REMATCH[1]}" == "$wanted" ]]; then
      printf '%s=%s\n' "$wanted" "$replacement" >> "$temporary"
      found=1
    else
      printf '%s\n' "$line" >> "$temporary"
    fi
  done < "$ENV_FILE"

  if (( found == 0 )); then
    printf '\n%s=%s\n' "$wanted" "$replacement" >> "$temporary"
  fi
  chmod 600 "$temporary"
  mv -f -- "$temporary" "$ENV_FILE"
}

ensure_env_file() {
  if [[ -f "$ENV_FILE" ]]; then
    chmod 600 "$ENV_FILE"
    log "Using environment file: $ENV_FILE"
    return
  fi
  [[ -f "$SCRIPT_DIR/.env.example" ]] || fail "Missing $ENV_FILE and $SCRIPT_DIR/.env.example"
  cp "$SCRIPT_DIR/.env.example" "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  ENV_FILE_CREATED=1
  log "Created $ENV_FILE from .env.example."
}

ensure_env_setting() {
  local name="$1"
  local default_value="$2"
  if read_env_value "$name" >/dev/null 2>&1; then
    return
  fi
  set_env_value "$name" "$default_value"
  log "Added $name to $ENV_FILE."
}

ensure_admin_settings() {
  ensure_env_setting SYSTEM_ADMIN_EMAIL ""
  ensure_env_setting SYSTEM_ADMIN_PASSWORD ""
  ensure_env_setting SYSTEM_ADMIN_DISPLAY_NAME "WenzWork Administrator"
}

set_host_default() {
  local name="$1"
  local value="$2"
  if [[ ! -v $name ]] || [[ -z "${!name}" ]]; then
    printf -v "$name" '%s' "$value"
    export "${name?}"
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
  # PUBLIC_BASE_URL is assigned dynamically by set_host_default above.
  # shellcheck disable=SC2153
  set_host_default ALLOWED_ORIGINS "$PUBLIC_BASE_URL"
  set_host_default HOST_SECRETS_FILE "$SCRIPT_DIR/cache/host-secrets/application.env"
  set_host_default RELEASE_ASSET_CACHE_DIR "$SCRIPT_DIR/cache/releases"
  set_host_default GITHUB_RELEASE_REPOSITORY lyming99/wenzwork_web
  set_host_default RELAY_DEVELOPMENT_CA_DIR "$SCRIPT_DIR/cache/host-secrets/relay-ca"
  set_host_default RELAY_BOOTSTRAP_ASSETS_DIR "$SCRIPT_DIR/relay-bootstrap"
  set_host_default REMOTE_MVP_ENABLED true
}

is_placeholder() {
  local value
  value="$(trim_whitespace "${1:-}")"
  [[ -z "$value" || "$value" == \<*\> || "$value" == *generate-* || "$value" == *replace-* ]]
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "Required command not found: $1"
}

require_runtime_files() {
  [[ -x "$ADMIN_BIN" ]] || fail "Administrator binary is missing or not executable: $ADMIN_BIN"
  [[ -x "$MIGRATE_BIN" ]] || fail "Migration binary is missing or not executable: $MIGRATE_BIN"
  [[ -d "$SCRIPT_DIR/migrations" ]] || fail "Migration directory is missing: $SCRIPT_DIR/migrations"
}

require_value() {
  local name="$1"
  local value="${!name:-}"
  if is_placeholder "$value"; then
    fail "$name must be configured in $ENV_FILE"
  fi
}

generate_dependency_password() {
  local value
  [[ -r /dev/urandom ]] || fail "/dev/urandom is required to generate dependency credentials."
  value="$(LC_ALL=C od -An -N24 -tx1 /dev/urandom | tr -d '[:space:]')"
  [[ "$value" =~ ^[0-9a-f]{48}$ ]] || fail "Could not generate a secure dependency credential."
  printf '%s' "$value"
}

provision_host_dependencies() {
  local database_password redis_password reply attempt
  if [[ -n "${DATABASE_URL:-}" && -n "${REDIS_URL:-}" ]]; then
    return
  fi
  if [[ -n "${DATABASE_URL:-}" || -n "${REDIS_URL:-}" ]]; then
    fail "Configure DATABASE_URL and REDIS_URL together, or leave both empty for managed Docker services."
  fi
  require_command docker
  ! docker container inspect wenzwork-postgres >/dev/null 2>&1 ||
    fail "wenzwork-postgres already exists but DATABASE_URL is missing; recover its credential or remove the stale container."
  ! docker container inspect wenzwork-redis >/dev/null 2>&1 ||
    fail "wenzwork-redis already exists but REDIS_URL is missing; recover its credential or remove the stale container."

  database_password="$(generate_dependency_password)"
  redis_password="$(generate_dependency_password)"
  log "Starting managed PostgreSQL and Redis containers..."
  docker run -d --name wenzwork-postgres --restart unless-stopped \
    -e POSTGRES_USER=wenzwork -e "POSTGRES_PASSWORD=$database_password" -e POSTGRES_DB=wenzwork \
    -p 127.0.0.1:54328:5432 -v wenzwork-postgres-data:/var/lib/postgresql/data \
    postgres:17-alpine >/dev/null
  docker run -d --name wenzwork-redis --restart unless-stopped \
    -p 127.0.0.1:63798:6379 -v wenzwork-redis-data:/data redis:8-alpine \
    redis-server --appendonly yes --requirepass "$redis_password" >/dev/null
  DATABASE_URL="postgres://wenzwork:$database_password@127.0.0.1:54328/wenzwork?sslmode=disable"
  REDIS_URL="redis://:$redis_password@127.0.0.1:63798/0"
  set_env_value DATABASE_URL "$DATABASE_URL"
  set_env_value REDIS_URL "$REDIS_URL"
  export DATABASE_URL REDIS_URL

  for ((attempt = 1; attempt <= 60; attempt++)); do
    docker exec wenzwork-postgres pg_isready -U wenzwork -d wenzwork >/dev/null 2>&1 && break
    sleep 1
  done
  docker exec wenzwork-postgres pg_isready -U wenzwork -d wenzwork >/dev/null 2>&1 ||
    fail "Managed PostgreSQL did not become ready."
  for ((attempt = 1; attempt <= 60; attempt++)); do
    reply="$(docker exec -e "REDISCLI_AUTH=$redis_password" wenzwork-redis \
      redis-cli -h 127.0.0.1 -p 6379 ping 2>/dev/null || true)"
    [[ "$reply" == PONG ]] && break
    sleep 1
  done
  [[ "$reply" == PONG ]] || fail "Managed Redis did not become ready."
}

validate_email() {
  local name="$1"
  local value="$2"
  [[ "$value" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]] ||
    fail "$name must be a valid email address."
}

load_admin_config() {
  SYSTEM_ADMIN_EMAIL="${SYSTEM_ADMIN_EMAIL:-${BOOTSTRAP_ADMIN_EMAIL:-}}"
  SYSTEM_ADMIN_PASSWORD="${SYSTEM_ADMIN_PASSWORD:-${BOOTSTRAP_ADMIN_PASSWORD:-}}"
  SYSTEM_ADMIN_DISPLAY_NAME="${SYSTEM_ADMIN_DISPLAY_NAME:-${BOOTSTRAP_ADMIN_DISPLAY_NAME:-WenzWork Administrator}}"

  export SYSTEM_ADMIN_EMAIL SYSTEM_ADMIN_PASSWORD SYSTEM_ADMIN_DISPLAY_NAME
}

require_new_env_admin_settings() {
  if (( ENV_FILE_CREATED == 1 )) &&
    { is_placeholder "$SYSTEM_ADMIN_EMAIL" || is_placeholder "$SYSTEM_ADMIN_PASSWORD"; }; then
    fail "Set SYSTEM_ADMIN_EMAIL and SYSTEM_ADMIN_PASSWORD in $ENV_FILE, then run init_server.sh again."
  fi
}

validate_new_admin_config() {
  require_value SYSTEM_ADMIN_EMAIL
  require_value SYSTEM_ADMIN_PASSWORD
  validate_email SYSTEM_ADMIN_EMAIL "$SYSTEM_ADMIN_EMAIL"
  if (( ${#SYSTEM_ADMIN_PASSWORD} < 8 || ${#SYSTEM_ADMIN_PASSWORD} > 128 )); then
    fail "SYSTEM_ADMIN_PASSWORD must contain between 8 and 128 characters."
  fi
  [[ "$SYSTEM_ADMIN_PASSWORD" != *$'\n'* && "$SYSTEM_ADMIN_PASSWORD" != *$'\r'* ]] ||
    fail "SYSTEM_ADMIN_PASSWORD must not contain line breaks."
}

detect_admin_state() {
  local status existing_email
  log "Checking whether a system administrator already exists..."
  if ! status="$(cd "$SCRIPT_DIR" && "$ADMIN_BIN" bootstrap status)"; then
    fail "Could not query the system administrator bootstrap status."
  fi
  # database.Open writes slow-query diagnostics to stdout. Keep the command's
  # final machine-readable line if such a diagnostic precedes it.
  status="${status##*$'\n'}"

  case "$status" in
    uninitialized)
      validate_new_admin_config
      ADMIN_ALREADY_INITIALIZED=0
      ;;
    initialized$'\t'*)
      existing_email="${status#*$'\t'}"
      validate_email existing_system_administrator "$existing_email"
      if is_placeholder "$SYSTEM_ADMIN_EMAIL"; then
        set_env_value SYSTEM_ADMIN_EMAIL "$existing_email"
        SYSTEM_ADMIN_EMAIL="$existing_email"
        export SYSTEM_ADMIN_EMAIL
        log "Saved the existing system administrator email to $ENV_FILE."
      elif [[ "${SYSTEM_ADMIN_EMAIL,,}" != "${existing_email,,}" ]]; then
        fail "SYSTEM_ADMIN_EMAIL does not match the existing super administrator: $existing_email"
      fi
      ADMIN_ALREADY_INITIALIZED=1
      log "System administrator is already initialized: $existing_email"
      ;;
    *)
      fail "Unexpected administrator bootstrap status: $status"
      ;;
  esac
}

check_smtp() {
  local recipient

  require_value SMTP_HOST
  require_value SMTP_PORT
  require_value MAIL_FROM
  [[ "$SMTP_PORT" =~ ^[0-9]+$ && "$SMTP_PORT" -ge 1 && "$SMTP_PORT" -le 65535 ]] ||
    fail "SMTP_PORT must be an integer between 1 and 65535."
  [[ "$SMTP_HOST" =~ ^[A-Za-z0-9._:-]+$ ]] || fail "SMTP_HOST contains unsupported characters."
  if [[ -n "${SMTP_USER:-}" && -z "${SMTP_PASSWORD:-}" ]]; then
    fail "SMTP_PASSWORD is required when SMTP_USER is configured."
  fi

  recipient="$SYSTEM_ADMIN_EMAIL"
  validate_email SYSTEM_ADMIN_EMAIL "$recipient"

  log "Checking SMTP delivery to $recipient..."
  if ! (cd "$SCRIPT_DIR" && "$ADMIN_BIN" smtp test --env-file "$ENV_FILE"); then
    fail "SMTP check failed through the WenzWork Go mail sender."
  fi
  log "SMTP check succeeded; a test message was accepted for $recipient."
}

check_s3() {
  local name configured=0
  for name in S3_ENDPOINT S3_REGION S3_BUCKET S3_ACCESS_KEY_ID S3_SECRET_ACCESS_KEY; do
    if ! is_placeholder "${!name:-}"; then
      configured=$((configured + 1))
    fi
  done
  if (( configured == 0 )); then
    log "S3 is not configured; skipping the S3 check. GitHub-backed release downloads remain available."
    return
  fi
  for name in S3_ENDPOINT S3_REGION S3_BUCKET S3_ACCESS_KEY_ID S3_SECRET_ACCESS_KEY; do
    require_value "$name"
  done
  log "Checking S3 write access for bucket $S3_BUCKET..."
  if ! (cd "$SCRIPT_DIR" && "$ADMIN_BIN" s3 test --env-file "$ENV_FILE"); then
    fail "S3 check failed through the WenzWork Go S3 client."
  fi
  log "S3 write, read, and delete checks succeeded for bucket $S3_BUCKET."
}

run_migrations() {
  require_value DATABASE_URL
  require_value REDIS_URL
  log "Applying database migrations..."
  (cd "$SCRIPT_DIR" && "$MIGRATE_BIN" up)
}

initialize_admin() {
  if (( ADMIN_ALREADY_INITIALIZED == 1 )); then
    log "System administrator creation is not required."
    return
  fi
  log "Checking the first system administrator: $SYSTEM_ADMIN_EMAIL"
  if ! (
    cd "$SCRIPT_DIR"
    BOOTSTRAP_ADMIN_EMAIL="$SYSTEM_ADMIN_EMAIL" \
      BOOTSTRAP_ADMIN_PASSWORD="$SYSTEM_ADMIN_PASSWORD" \
      BOOTSTRAP_ADMIN_DISPLAY_NAME="$SYSTEM_ADMIN_DISPLAY_NAME" \
      "$ADMIN_BIN" bootstrap
  ); then
    fail "System administrator initialization failed. If another super administrator exists, SYSTEM_ADMIN_EMAIL must match it."
  fi
}

usage() {
  cat <<'EOF'
Usage: ./init_server.sh

Creates the core Host .env when needed, lets Host generate its protected
runtime secrets, applies database migrations, and creates the first system
administrator from SYSTEM_ADMIN_* settings. Database, Redis, email, and public
URL settings are finalized in the first-login system setup page.
EOF
}

main() {
  case "${1:-}" in
    -h | --help | help)
      usage
      return
      ;;
    "")
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac

  ensure_env_file
  ensure_admin_settings
  load_env
  apply_host_defaults
  # This credential belongs to the standalone Release upgrader and should not
  # be inherited by database, SMTP, S3, or administrator initialization tools.
  unset GITHUB_ACCESS_TOKEN
  require_command mktemp
  require_runtime_files
  load_admin_config
  require_new_env_admin_settings
  provision_host_dependencies
  run_migrations
  detect_admin_state
  initialize_admin
  log "Initialization completed successfully. You can now run ./start_server.sh"
}

main "$@"
