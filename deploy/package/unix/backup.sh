#!/usr/bin/env bash

set -Eeuo pipefail
umask 077

PACKAGE_ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=lib/common.sh
source "$PACKAGE_ROOT/runtime/lib/common.sh"

POSTGRES_CONTAINER=wenzwork-postgres
POSTGRES_DATA_DIR=/var/lib/postgresql/data

operation=''
temporary=''
helper_id=''
container_image=''
rollback_archive=''
host_was_running=0
host_stopped=0
postgres_was_running=0
postgres_stopped=0
volume_mutated=0
operation_completed=0
preserve_temporary=0

usage() {
  cat <<'EOF'
Usage:
  ./backup.sh backup [OUTPUT.tar.gz]
  ./backup.sh restore BACKUP.tar.gz --confirm

Creates or restores a cold archive of the Docker volume mounted at
/var/lib/postgresql/data in the managed wenzwork-postgres container. The
default file name follows the 1Panel-style format:
postgresql_YYYYMMDDHHMMSSxxxxx.tar.gz

Host and PostgreSQL are stopped while the volume is archived or replaced.
Restore first creates an internal rollback archive and automatically restores
it if extraction or startup fails. The archive contains container volume data
only; it does not include .env, Host secrets, Redis, logs, or binaries.
EOF
}

remove_helper() {
  if [[ -n "$helper_id" ]]; then
    docker rm -f "$helper_id" >/dev/null 2>&1 || true
    helper_id=''
  fi
}

detect_host_state() {
  local pid_file="$PACKAGE_ROOT/runtime/pids/wenzwork.pid" pid
  host_was_running=0
  [[ -f "$pid_file" ]] || return 0
  pid="$(tr -d '[:space:]' < "$pid_file")"
  if [[ "$pid" =~ ^[1-9][0-9]*$ ]] && kill -0 "$pid" 2>/dev/null; then
    host_was_running=1
  fi
}

detect_postgres_state() {
  [[ "$(docker inspect -f '{{.State.Running}}' "$POSTGRES_CONTAINER" 2>/dev/null)" == true ]]
}

wait_for_postgres() {
  local attempt
  for ((attempt = 1; attempt <= 60; attempt++)); do
    docker exec "$POSTGRES_CONTAINER" pg_isready -U wenzwork -d wenzwork >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

stop_runtime() {
  detect_host_state
  if (( host_was_running == 1 )); then
    "$PACKAGE_ROOT/stop.sh"
    host_stopped=1
  elif [[ -f "$PACKAGE_ROOT/runtime/pids/wenzwork.pid" ]]; then
    "$PACKAGE_ROOT/stop.sh"
  fi

  if detect_postgres_state; then
    postgres_was_running=1
    package_log "Stopping $POSTGRES_CONTAINER for a consistent volume snapshot..."
    docker stop -t 60 "$POSTGRES_CONTAINER" >/dev/null
    postgres_stopped=1
  fi
}

restore_runtime_state() {
  if (( postgres_was_running == 1 && postgres_stopped == 1 )); then
    docker start "$POSTGRES_CONTAINER" >/dev/null || return 1
    postgres_stopped=0
    wait_for_postgres || return 1
  fi
  if (( host_was_running == 1 && host_stopped == 1 )); then
    "$PACKAGE_ROOT/start.sh" --background || return 1
    host_stopped=0
  fi
}

create_volume_archive() {
  local output="$1"
  remove_helper
  helper_id="$(docker create --volumes-from "$POSTGRES_CONTAINER:ro" "$container_image" \
    sh -ceu 'test -f /var/lib/postgresql/data/PG_VERSION; tar -czf /tmp/postgresql.tar.gz -C /var/lib/postgresql/data .')" || return 1
  [[ -n "$helper_id" ]] || return 1
  docker start -a "$helper_id" >/dev/null || return 1
  docker cp "$helper_id:/tmp/postgresql.tar.gz" "$output" >/dev/null || return 1
  remove_helper
  [[ -s "$output" ]]
}

replace_volume_from_archive() {
  local archive="$1"
  remove_helper
  helper_id="$(docker create --volumes-from "$POSTGRES_CONTAINER" "$container_image" sh -ceu '
    data=/var/lib/postgresql/data
    find "$data" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} \;
    tar -xzf /tmp/postgresql.tar.gz -C "$data"
    test -f "$data/PG_VERSION"
  ')" || return 1
  [[ -n "$helper_id" ]] || return 1
  docker cp "$archive" "$helper_id:/tmp/postgresql.tar.gz" >/dev/null || return 1
  docker start -a "$helper_id" >/dev/null || return 1
  remove_helper
}

read_archive_pg_version() {
  local archive="$1" value
  value="$(tar -xOzf "$archive" ./PG_VERSION 2>/dev/null || tar -xOzf "$archive" PG_VERSION 2>/dev/null || true)"
  value="$(package_trim "$value")"
  [[ "$value" =~ ^[0-9]+$ ]] || package_die "backup archive has no valid PG_VERSION"
  printf '%s' "$value"
}

read_volume_pg_version() {
  local value
  remove_helper
  helper_id="$(docker create --volumes-from "$POSTGRES_CONTAINER:ro" "$container_image" \
    sh -ceu 'cat /var/lib/postgresql/data/PG_VERSION')" || return 1
  [[ -n "$helper_id" ]] || return 1
  value="$(docker start -a "$helper_id")" || return 1
  remove_helper
  value="$(package_trim "$value")"
  [[ "$value" =~ ^[0-9]+$ ]] || package_die "managed PostgreSQL volume has no valid PG_VERSION"
  printf '%s' "$value"
}

validate_volume_archive() {
  local archive="$1" entry type current_version archive_version
  [[ -f "$archive" && ! -L "$archive" ]] || package_die "backup archive is missing or is a symbolic link: $archive"
  [[ "$archive" == *.tar.gz ]] || package_die "backup archive must use the .tar.gz extension"
  tar -tzf "$archive" >/dev/null || package_die "backup archive is not a readable tar.gz file"

  while IFS= read -r entry; do
    entry="${entry#./}"
    [[ -z "$entry" ]] && continue
    case "$entry" in
      /* | ../* | */../* | */.. | *\\*) package_die "unsafe backup archive entry: $entry" ;;
    esac
  done < <(tar -tzf "$archive")
  while IFS= read -r type; do
    case "$type" in
      - | d) ;;
      *) package_die "backup archive contains a link or special file" ;;
    esac
  done < <(tar -tvzf "$archive" | sed -n 's/^\(.\).*$/\1/p')

  archive_version="$(read_archive_pg_version "$archive")"
  current_version="$(read_volume_pg_version)"
  [[ "$archive_version" == "$current_version" ]] ||
    package_die "PostgreSQL major version mismatch: archive=$archive_version container=$current_version"
}

rollback_failed_restore() {
  set +e
  package_log "Restore failed; restoring the original PostgreSQL container volume..."
  if detect_postgres_state; then
    docker stop -t 60 "$POSTGRES_CONTAINER" >/dev/null 2>&1 || return 1
  fi
  postgres_stopped=1
  replace_volume_from_archive "$rollback_archive" || return 1
  volume_mutated=0
  if (( postgres_was_running == 1 )); then
    docker start "$POSTGRES_CONTAINER" >/dev/null || return 1
    postgres_stopped=0
    wait_for_postgres || return 1
  fi
  if (( host_was_running == 1 )); then
    "$PACKAGE_ROOT/start.sh" --background >/dev/null 2>&1 || return 1
    host_stopped=0
  fi
  return 0
}

cleanup() {
  local status=$?
  trap - EXIT HUP INT TERM
  set +e
  remove_helper
  if (( status != 0 && operation_completed == 0 )); then
    if [[ "$operation" == restore && $volume_mutated -eq 1 && -s "$rollback_archive" ]]; then
      if ! rollback_failed_restore; then
        preserve_temporary=1
        package_log "ERROR: automatic volume rollback failed; keep Host stopped and recover $rollback_archive manually"
      fi
    else
      restore_runtime_state ||
        package_log "ERROR: the original containers could not be returned to their previous state"
    fi
  fi
  remove_helper
  if (( preserve_temporary == 0 )); then
    [[ -z "$temporary" ]] || rm -rf -- "${temporary:?}"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

require_managed_postgres() {
  package_validate_tree "$PACKAGE_ROOT"
  package_load_metadata "$PACKAGE_ROOT"
  [[ "$WENZWORK_PACKAGE_COMPONENT" == host ]] ||
    package_die "backup.sh is available only in a Host deployment package"
  package_create_runtime_directories "$PACKAGE_ROOT"
  package_load_env "$PACKAGE_ROOT/.env"
  package_apply_component_defaults "$PACKAGE_ROOT"
  unset GITHUB_ACCESS_TOKEN GH_TOKEN GITHUB_TOKEN

  command -v docker >/dev/null 2>&1 || package_die "docker is required"
  command -v tar >/dev/null 2>&1 || package_die "tar is required"
  [[ "${DATABASE_URL:-}" =~ ^postgres(ql)?://wenzwork:[^@]+@(127\.0\.0\.1|localhost):54328/wenzwork(\?.*)?$ ]] ||
    package_die "DATABASE_URL does not point to the managed wenzwork-postgres container"
  docker container inspect "$POSTGRES_CONTAINER" >/dev/null 2>&1 ||
    package_die "managed PostgreSQL container $POSTGRES_CONTAINER does not exist"
  container_image="$(docker inspect -f '{{.Config.Image}}' "$POSTGRES_CONTAINER")"
  [[ -n "$container_image" ]] || package_die "could not resolve the PostgreSQL container image"
  read_volume_pg_version >/dev/null
}

new_backup_name() {
  local timestamp random
  timestamp="$(date +%Y%m%d%H%M%S)"
  random="$(LC_ALL=C od -An -N4 -tx1 /dev/urandom | tr -d '[:space:]' | cut -c1-5)"
  [[ "$random" =~ ^[0-9a-f]{5}$ ]] || package_die "could not generate a backup suffix"
  printf 'postgresql_%s%s.tar.gz' "$timestamp" "$random"
}

backup_volume() {
  local requested="${1:-}" parent output staged hash
  if [[ -z "$requested" ]]; then
    requested="$PACKAGE_ROOT/cache/backups/$(new_backup_name)"
  fi
  [[ "$requested" == *.tar.gz ]] || package_die "backup output must use the .tar.gz extension"
  parent="$(dirname -- "$requested")"
  mkdir -p -- "$parent"
  parent="$(CDPATH='' cd -- "$parent" && pwd -P)"
  output="$parent/$(basename -- "$requested")"
  [[ ! -e "$output" && ! -L "$output" ]] || package_die "backup output already exists: $output"

  temporary="$(mktemp -d "$parent/.wenzwork-volume-backup.XXXXXX")"
  staged="$temporary/postgresql.tar.gz"
  stop_runtime
  package_log "Archiving the $POSTGRES_CONTAINER data volume..."
  create_volume_archive "$staged" || package_die "could not archive the PostgreSQL container volume"
  validate_volume_archive "$staged"
  mv -- "$staged" "$output"
  chmod 0600 "$output"
  restore_runtime_state || package_die "backup was created, but the original runtime could not be restarted"
  operation_completed=1
  hash="$(package_sha256 "$output")"
  package_log "PostgreSQL container volume backup created: $output"
  package_log "SHA-256: $hash"
}

restore_volume() {
  local requested="$1" current_version
  [[ "$2" == --confirm ]] || package_die "restore requires --confirm"
  requested="$(CDPATH='' cd -- "$(dirname -- "$requested")" && pwd -P)/$(basename -- "$requested")"
  validate_volume_archive "$requested"
  current_version="$(read_archive_pg_version "$requested")"
  temporary="$(mktemp -d "$PACKAGE_ROOT/cache/backups/.wenzwork-volume-restore.XXXXXX")"
  rollback_archive="$temporary/postgresql-before-restore.tar.gz"

  stop_runtime
  package_log "Creating an automatic rollback snapshot of the current container volume..."
  create_volume_archive "$rollback_archive" || package_die "could not create the pre-restore rollback snapshot"
  validate_volume_archive "$rollback_archive"

  package_log "Replacing the $POSTGRES_CONTAINER data volume from $(basename -- "$requested")..."
  volume_mutated=1
  replace_volume_from_archive "$requested"
  docker start "$POSTGRES_CONTAINER" >/dev/null
  postgres_stopped=0
  wait_for_postgres || package_die "restored PostgreSQL container did not become ready"
  [[ "$(docker exec "$POSTGRES_CONTAINER" cat "$POSTGRES_DATA_DIR/PG_VERSION")" == "$current_version" ]] ||
    package_die "restored PostgreSQL volume version verification failed"

  if (( host_was_running == 1 )); then
    "$PACKAGE_ROOT/start.sh" --background
    host_stopped=0
  fi
  if (( postgres_was_running == 0 )); then
    docker stop -t 60 "$POSTGRES_CONTAINER" >/dev/null
    postgres_stopped=1
  fi
  volume_mutated=0
  operation_completed=1
  package_log "PostgreSQL container volume restored from $requested"
}

case "${1:-backup}" in
  -h | --help | help)
    usage
    exit 0
    ;;
  backup)
    (( $# <= 2 )) || { usage >&2; exit 2; }
    operation=backup
    require_managed_postgres
    backup_volume "${2:-}"
    ;;
  restore)
    (( $# == 3 )) || { usage >&2; exit 2; }
    operation=restore
    require_managed_postgres
    restore_volume "$2" "$3"
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
