#!/usr/bin/env bash

set -Eeuo pipefail

REPOSITORY_ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/wenzwork-container-backup-test.XXXXXX")"
PACKAGE_ROOT="$TEST_ROOT/package"
FAKE_BIN="$TEST_ROOT/bin"
VOLUME_ROOT="$TEST_ROOT/volume"
HELPER_ROOT="$TEST_ROOT/helpers"
DOCKER_STATE="$TEST_ROOT/postgres-running"
DOCKER_LOG="$TEST_ROOT/docker.log"

cleanup() {
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT

mkdir -p "$PACKAGE_ROOT/bin" "$PACKAGE_ROOT/config" "$PACKAGE_ROOT/runtime/lib" \
  "$PACKAGE_ROOT/runtime/pids" "$PACKAGE_ROOT/workspace" "$PACKAGE_ROOT/cache/backups" \
  "$FAKE_BIN" "$VOLUME_ROOT/base" "$HELPER_ROOT"
cp "$REPOSITORY_ROOT/deploy/package/unix/backup.sh" "$PACKAGE_ROOT/backup.sh"
cp "$REPOSITORY_ROOT/deploy/package/unix/lib/common.sh" "$PACKAGE_ROOT/runtime/lib/common.sh"
printf '17\n' > "$VOLUME_ROOT/PG_VERSION"
printf 'original container volume data\n' > "$VOLUME_ROOT/base/data.txt"
touch "$DOCKER_STATE"

cat > "$PACKAGE_ROOT/config/package.env" <<'EOF'
WENZWORK_PACKAGE_COMPONENT=host
WENZWORK_PACKAGE_PLATFORM=linux
WENZWORK_PACKAGE_ARCHITECTURE=amd64
WENZWORK_PACKAGE_VERSION=vtest
WENZWORK_PACKAGE_ASSET_BASENAME=wenzwork-host-deployment
WENZWORK_PACKAGE_CHECKSUM_ASSET=DEPLOYMENT-SHA256SUMS
WENZWORK_GITHUB_REPOSITORY=owner/repository
EOF
cat > "$PACKAGE_ROOT/config/host.env.example" <<'EOF'
SYSTEM_ADMIN_EMAIL=
SYSTEM_ADMIN_PASSWORD=
SYSTEM_SETUP_COMPLETED=false
EOF
cat > "$PACKAGE_ROOT/.env" <<'EOF'
SYSTEM_ADMIN_EMAIL=admin@example.test
SYSTEM_ADMIN_PASSWORD=test-password
SYSTEM_SETUP_COMPLETED=false
DATABASE_URL=postgres://wenzwork:0123456789abcdef@127.0.0.1:54328/wenzwork?sslmode=disable
REDIS_URL=redis://:0123456789abcdef@127.0.0.1:63798/0
EOF
for name in start.sh stop.sh upgrade.sh; do
  cat > "$PACKAGE_ROOT/$name" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
case "$(basename -- "$0")" in
  stop.sh) rm -f -- "$(dirname -- "$0")/runtime/pids/wenzwork.pid" ;;
esac
EOF
done
printf '#!/usr/bin/env bash\nexit 0\n' > "$PACKAGE_ROOT/bin/wenzwork-api"
printf '{}\n' > "$PACKAGE_ROOT/PACKAGE-MANIFEST.json"
printf 'vtest\n' > "$PACKAGE_ROOT/VERSION"
chmod 0755 "$PACKAGE_ROOT/backup.sh" "$PACKAGE_ROOT"/*.sh "$PACKAGE_ROOT/bin/wenzwork-api"
chmod 0600 "$PACKAGE_ROOT/.env"

cat > "$FAKE_BIN/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

printf '%q ' "$@" >> "$WENZWORK_TEST_DOCKER_LOG"
printf '\n' >> "$WENZWORK_TEST_DOCKER_LOG"
[[ " $* " != *' pg_dump '* && " $* " != *' pg_restore '* ]]

command_name="${1:-}"
shift || true
case "$command_name" in
  container)
    [[ "${1:-}" == inspect && "${2:-}" == wenzwork-postgres ]]
    ;;
  inspect)
    [[ "${1:-}" == -f ]]
    format="$2"
    case "$format" in
      '{{.State.Running}}') [[ -f "$WENZWORK_TEST_DOCKER_STATE" ]] && printf 'true\n' || printf 'false\n' ;;
      '{{.Config.Image}}') printf 'postgres:17-alpine\n' ;;
      *) exit 2 ;;
    esac
    ;;
  create)
    counter_file="$WENZWORK_TEST_HELPER_ROOT/counter"
    counter=0
    [[ ! -f "$counter_file" ]] || counter="$(< "$counter_file")"
    counter=$((counter + 1))
    printf '%s\n' "$counter" > "$counter_file"
    helper="helper$counter"
    mkdir -p "$WENZWORK_TEST_HELPER_ROOT/$helper"
    printf '%s\n' "$*" > "$WENZWORK_TEST_HELPER_ROOT/$helper/command"
    printf '%s\n' "$helper"
    ;;
  start)
    if [[ "${1:-}" == -a ]]; then
      helper="$2"
      helper_dir="$WENZWORK_TEST_HELPER_ROOT/$helper"
      helper_command="$(< "$helper_dir/command")"
      if [[ "$helper_command" == *'cat /var/lib/postgresql/data/PG_VERSION'* ]]; then
        cat "$WENZWORK_TEST_VOLUME_ROOT/PG_VERSION"
      elif [[ "$helper_command" == *'tar -czf /tmp/postgresql.tar.gz'* ]]; then
        (cd "$WENZWORK_TEST_VOLUME_ROOT" && tar -czf "$helper_dir/postgresql.tar.gz" .)
      elif [[ "$helper_command" == *'find "$data"'* ]]; then
        if [[ -f "$WENZWORK_TEST_ROOT/fail-next-restore" || -f "$WENZWORK_TEST_ROOT/fail-all-restores" ]]; then
          rm -f "$WENZWORK_TEST_ROOT/fail-next-restore"
          find "$WENZWORK_TEST_VOLUME_ROOT" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} \;
          printf 'partial\n' > "$WENZWORK_TEST_VOLUME_ROOT/partial"
          exit 1
        fi
        find "$WENZWORK_TEST_VOLUME_ROOT" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} \;
        tar -xzf "$helper_dir/postgresql.tar.gz" -C "$WENZWORK_TEST_VOLUME_ROOT"
      else
        printf 'unknown helper command: %s\n' "$helper_command" >&2
        exit 2
      fi
    else
      [[ "${1:-}" == wenzwork-postgres ]]
      touch "$WENZWORK_TEST_DOCKER_STATE"
      printf 'wenzwork-postgres\n'
    fi
    ;;
  stop)
    [[ "${1:-}" == -t ]]
    shift 2
    [[ "${1:-}" == wenzwork-postgres ]]
    rm -f "$WENZWORK_TEST_DOCKER_STATE"
    printf 'wenzwork-postgres\n'
    ;;
  cp)
    source_path="$1"
    destination_path="$2"
    if [[ "$source_path" == helper*:/tmp/postgresql.tar.gz ]]; then
      helper="${source_path%%:*}"
      cp "$WENZWORK_TEST_HELPER_ROOT/$helper/postgresql.tar.gz" "$destination_path"
    elif [[ "$destination_path" == helper*:/tmp/postgresql.tar.gz ]]; then
      helper="${destination_path%%:*}"
      cp "$source_path" "$WENZWORK_TEST_HELPER_ROOT/$helper/postgresql.tar.gz"
    else
      exit 2
    fi
    ;;
  rm)
    [[ "${1:-}" == -f ]]
    rm -rf -- "$WENZWORK_TEST_HELPER_ROOT/$2"
    ;;
  exec)
    [[ "${1:-}" == wenzwork-postgres ]]
    shift
    case "${1:-}" in
      pg_isready) [[ -f "$WENZWORK_TEST_DOCKER_STATE" ]] ;;
      cat) cat "$WENZWORK_TEST_VOLUME_ROOT/PG_VERSION" ;;
      *) exit 2 ;;
    esac
    ;;
  *)
    printf 'unexpected docker command: %s\n' "$command_name" >&2
    exit 2
    ;;
esac
EOF
chmod 0755 "$FAKE_BIN/docker"

export WENZWORK_TEST_ROOT="$TEST_ROOT"
export WENZWORK_TEST_VOLUME_ROOT="$VOLUME_ROOT"
export WENZWORK_TEST_HELPER_ROOT="$HELPER_ROOT"
export WENZWORK_TEST_DOCKER_STATE="$DOCKER_STATE"
export WENZWORK_TEST_DOCKER_LOG="$DOCKER_LOG"
export PATH="$FAKE_BIN:$PATH"

"$PACKAGE_ROOT/backup.sh" backup
mapfile -t backups < <(find "$PACKAGE_ROOT/cache/backups" -maxdepth 1 -type f -name 'postgresql_*.tar.gz')
(( ${#backups[@]} == 1 ))
backup="${backups[0]}"
[[ "$(basename -- "$backup")" =~ ^postgresql_[0-9]{14}[0-9a-f]{5}\.tar\.gz$ ]]
tar -tzf "$backup" >/dev/null
grep -Fxq 'original container volume data' "$VOLUME_ROOT/base/data.txt"
[[ -f "$DOCKER_STATE" ]]

printf 'changed container volume data\n' > "$VOLUME_ROOT/base/data.txt"
"$PACKAGE_ROOT/backup.sh" restore "$backup" --confirm
grep -Fxq 'original container volume data' "$VOLUME_ROOT/base/data.txt"
[[ -f "$DOCKER_STATE" ]]

printf 'current data before failed restore\n' > "$VOLUME_ROOT/base/data.txt"
touch "$TEST_ROOT/fail-next-restore"
if "$PACKAGE_ROOT/backup.sh" restore "$backup" --confirm > "$TEST_ROOT/failed-restore.log" 2>&1; then
  printf 'restore unexpectedly succeeded during injected volume replacement failure\n' >&2
  exit 1
fi
grep -Fxq 'current data before failed restore' "$VOLUME_ROOT/base/data.txt"
[[ -f "$DOCKER_STATE" ]]
grep -Fq 'restoring the original PostgreSQL container volume' "$TEST_ROOT/failed-restore.log"
! grep -Eq 'pg_dump|pg_restore' "$DOCKER_LOG"

printf 'data retained in the rollback snapshot\n' > "$VOLUME_ROOT/base/data.txt"
touch "$TEST_ROOT/fail-all-restores"
if "$PACKAGE_ROOT/backup.sh" restore "$backup" --confirm > "$TEST_ROOT/failed-rollback.log" 2>&1; then
  printf 'restore unexpectedly succeeded while replacement and rollback were both failing\n' >&2
  exit 1
fi
grep -Fq 'automatic volume rollback failed' "$TEST_ROOT/failed-rollback.log"
find "$PACKAGE_ROOT/cache/backups" -maxdepth 1 -type d -name '.wenzwork-volume-restore.*' | grep -q .
[[ ! -f "$DOCKER_STATE" ]]
! find "$HELPER_ROOT" -mindepth 1 -maxdepth 1 -type d | grep -q .

for backup_script in \
  "$REPOSITORY_ROOT/deploy/package/unix/backup.sh" \
  "$REPOSITORY_ROOT/deploy/package/windows/Backup.ps1"; do
  grep -Fq -- '--volumes-from' "$backup_script"
  ! grep -Eq 'pg_dump|pg_restore' "$backup_script"
done

grep -Fq 'REDISCLI_AUTH=$redis_password' "$REPOSITORY_ROOT/deploy/package/unix/start.sh"
grep -Fq 'redis-cli -h 127.0.0.1 -p 6379 ping' "$REPOSITORY_ROOT/deploy/package/unix/start.sh"
grep -Fq 'REDISCLI_AUTH=$redis_password' "$REPOSITORY_ROOT/deploy/init_server.sh"
grep -Fq 'redis-cli -h 127.0.0.1 -p 6379 ping' "$REPOSITORY_ROOT/deploy/init_server.sh"
grep -Fq 'REDISCLI_AUTH=$redisPassword' "$REPOSITORY_ROOT/deploy/package/windows/Init.ps1"
grep -Fq "redis-cli -h 127.0.0.1 -p 6379 ping" "$REPOSITORY_ROOT/deploy/package/windows/Init.ps1"

printf 'Host container-volume backup/restore and Redis readiness tests passed\n'
