#!/usr/bin/env bash

set -Eeuo pipefail

[[ $(uname -s) == Linux ]] || {
  printf 'host startup initialization test is Linux-only\n'
  exit 0
}

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/wenzwork-host-init-test.XXXXXX")"
PACKAGE_ROOT="$TEST_ROOT/package"
MIGRATION_COUNT="$TEST_ROOT/migration-count"
DOCKER_LOG="$TEST_ROOT/docker.log"
RUNTIME_ENV_LOG="$TEST_ROOT/runtime-env.log"
TOOLS_DIR="$TEST_ROOT/tools"

cleanup() {
  if [[ -f "$PACKAGE_ROOT/runtime/pids/wenzwork.pid" ]]; then
    kill "$(< "$PACKAGE_ROOT/runtime/pids/wenzwork.pid")" >/dev/null 2>&1 || true
  fi
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT HUP INT TERM

fail() {
  printf 'host startup initialization test failed: %s\n' "$*" >&2
  exit 1
}

# shellcheck source=unix/lib/common.sh
source "$SCRIPT_DIR/unix/lib/common.sh"
platform="$(package_host_platform)"
architecture="$(package_host_architecture)"

mkdir -p "$PACKAGE_ROOT/bin" "$PACKAGE_ROOT/config" "$PACKAGE_ROOT/runtime/lib" \
  "$PACKAGE_ROOT/logs" "$PACKAGE_ROOT/workspace" "$PACKAGE_ROOT/cache" \
  "$PACKAGE_ROOT/migrations" "$TOOLS_DIR"
cp "$SCRIPT_DIR/unix/start.sh" "$PACKAGE_ROOT/start.sh"
cp "$SCRIPT_DIR/unix/lib/common.sh" "$PACKAGE_ROOT/runtime/lib/common.sh"
cat > "$PACKAGE_ROOT/stop.sh" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
root="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)"
pid_file="$root/runtime/pids/wenzwork.pid"
[[ -f "$pid_file" ]] || exit 0
pid="$(< "$pid_file")"
kill "$pid" 2>/dev/null || true
for _ in {1..50}; do
  kill -0 "$pid" 2>/dev/null || break
  sleep 0.02
done
rm -f -- "$pid_file"
EOF
printf '#!/usr/bin/env bash\nexit 0\n' > "$PACKAGE_ROOT/upgrade.sh"
printf 'v1.0.0\n' > "$PACKAGE_ROOT/VERSION"
printf '{}\n' > "$PACKAGE_ROOT/PACKAGE-MANIFEST.json"

cat > "$PACKAGE_ROOT/config/package.env" <<EOF
WENZWORK_PACKAGE_COMPONENT=host
WENZWORK_PACKAGE_PLATFORM=$platform
WENZWORK_PACKAGE_ARCHITECTURE=$architecture
WENZWORK_PACKAGE_VERSION=v1.0.0
WENZWORK_PACKAGE_ASSET_BASENAME=wenzwork-host-deployment
WENZWORK_PACKAGE_CHECKSUM_ASSET=DEPLOYMENT-SHA256SUMS
WENZWORK_GITHUB_REPOSITORY=example/wenzwork
EOF
cat > "$PACKAGE_ROOT/config/host.env.example" <<'EOF'
SYSTEM_ADMIN_EMAIL=
SYSTEM_ADMIN_PASSWORD=
SYSTEM_SETUP_COMPLETED=false
EOF

write_host_environment() {
  local dependency_mode="$1" setup_state="${2:-false}" credentials_mode="${3:-configured}"
  {
    printf '# Operator comment that must survive start.sh byte-for-byte.\n'
    case "$credentials_mode" in
      initial)
        printf 'SYSTEM_ADMIN_EMAIL=\nSYSTEM_ADMIN_PASSWORD=\n'
        ;;
      completed)
        printf 'SYSTEM_ADMIN_EMAIL=admin@example.test\nSYSTEM_ADMIN_PASSWORD=\n'
        ;;
      configured)
        printf 'SYSTEM_ADMIN_EMAIL=admin@example.test\nSYSTEM_ADMIN_PASSWORD=administrator-password\n'
        ;;
      *) fail "unknown credential mode: $credentials_mode" ;;
    esac
    printf 'SYSTEM_SETUP_COMPLETED=%s\nADVANCED_SETTING=retain-me\n' "$setup_state"
  } > "$PACKAGE_ROOT/.env"
  if [[ "$dependency_mode" == external || "$dependency_mode" == partial ]]; then
    cat >> "$PACKAGE_ROOT/.env" <<'EOF'
DATABASE_URL=postgres://wenzwork:secret@127.0.0.1:54328/wenzwork?sslmode=disable
EOF
  fi
  if [[ "$dependency_mode" == external ]]; then
    cat >> "$PACKAGE_ROOT/.env" <<'EOF'
REDIS_URL=redis://:secret@127.0.0.1:63798/0
EOF
  fi
  chmod 0600 "$PACKAGE_ROOT/.env"
}

cat > "$PACKAGE_ROOT/bin/wenzwork-api" <<'EOF'
#!/usr/bin/env bash
printf '%s\t%s\t%s\n' "${APP_ENV:-}" "${COOKIE_SECURE:-}" "${ADMIN_MFA_REQUIRED:-}" >> "$WENZWORK_TEST_RUNTIME_ENV_LOG"
trap 'exit 0' TERM INT
while :; do sleep 1; done
EOF
cat > "$PACKAGE_ROOT/bin/wenzwork-migrate" <<'EOF'
#!/usr/bin/env bash
count=0
[[ ! -f "$WENZWORK_TEST_MIGRATION_COUNT" ]] || count="$(< "$WENZWORK_TEST_MIGRATION_COUNT")"
printf '%d\n' "$((count + 1))" > "$WENZWORK_TEST_MIGRATION_COUNT"
EOF
cat > "$PACKAGE_ROOT/bin/wenzwork-admin" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == bootstrap && "${2:-}" == status ]]; then
  printf 'initialized\tadmin@example.test\n'
  exit 0
fi
exit 0
EOF
cat > "$TOOLS_DIR/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$WENZWORK_TEST_DOCKER_LOG"
mode="${WENZWORK_TEST_DOCKER_MODE:-failure}"
if [[ "$mode" == success || "$mode" == postgres-only ]]; then
  if [[ "${1:-}" == container && "${2:-}" == inspect ]]; then
    exit 1
  fi
  if [[ "${1:-}" == run ]]; then
    if [[ "$mode" == postgres-only && " $* " == *' --name wenzwork-redis '* ]]; then
      exit 88
    fi
    exit 0
  fi
  if [[ "${1:-}" == exec ]]; then
    case " $* " in
      *' redis-cli '*) printf 'PONG\n' ;;
    esac
    exit 0
  fi
fi
exit 88
EOF
chmod 0755 "$PACKAGE_ROOT"/*.sh "$PACKAGE_ROOT/bin"/* "$TOOLS_DIR/docker"

export WENZWORK_TEST_MIGRATION_COUNT="$MIGRATION_COUNT"
export WENZWORK_TEST_DOCKER_LOG="$DOCKER_LOG"
export WENZWORK_TEST_RUNTIME_ENV_LOG="$RUNTIME_ENV_LOG"
export WENZWORK_TEST_DOCKER_MODE=failure
export PATH="$TOOLS_DIR:$PATH"

rm -f -- "$PACKAGE_ROOT/.env"
if "$PACKAGE_ROOT/start.sh" --background > "$TEST_ROOT/missing-environment.log" 2>&1; then
  fail 'startup continued immediately after creating a missing .env'
fi
grep -Fq 'Created ' "$TEST_ROOT/missing-environment.log" ||
  fail 'startup did not report creating the missing .env'
grep -Fq 'Edit ' "$TEST_ROOT/missing-environment.log" ||
  fail 'startup did not ask the operator to edit the newly created .env'
cmp -s "$PACKAGE_ROOT/config/host.env.example" "$PACKAGE_ROOT/.env" ||
  fail 'startup did not create .env from the Host template'
[[ "$(stat -c '%a' "$PACKAGE_ROOT/.env")" == 600 ]] ||
  fail 'startup did not protect the generated .env with mode 0600'
[[ ! -e "$DOCKER_LOG" ]] || fail 'environment creation unexpectedly invoked Docker'
[[ ! -e "$MIGRATION_COUNT" ]] || fail 'environment creation unexpectedly ran migrations'

write_host_environment empty false initial
cp "$PACKAGE_ROOT/.env" "$TEST_ROOT/initial-before.env"
initial_identity="$(stat -Lc '%d:%i' "$PACKAGE_ROOT/.env")"
if "$PACKAGE_ROOT/start.sh" --background > "$TEST_ROOT/initial.log" 2>&1; then
  fail 'startup accepted untouched initial administrator values'
fi
grep -Fq 'SYSTEM_ADMIN_EMAIL still has its initial value' "$TEST_ROOT/initial.log" ||
  fail 'startup did not compare administrator values with the initial template'
cmp -s "$TEST_ROOT/initial-before.env" "$PACKAGE_ROOT/.env" ||
  fail 'startup changed .env before rejecting untouched initial values'
[[ "$(stat -Lc '%d:%i' "$PACKAGE_ROOT/.env")" == "$initial_identity" ]] ||
  fail 'startup replaced .env while rejecting untouched initial values'
[[ ! -e "$DOCKER_LOG" ]] || fail 'untouched initial values unexpectedly invoked Docker'
[[ ! -e "$MIGRATION_COUNT" ]] || fail 'untouched initial values unexpectedly ran migrations'

write_host_environment external true completed
cp "$PACKAGE_ROOT/.env" "$TEST_ROOT/completed-before.env"
completed_identity="$(stat -Lc '%d:%i' "$PACKAGE_ROOT/.env")"
"$PACKAGE_ROOT/start.sh" --background > "$TEST_ROOT/first.log"
"$PACKAGE_ROOT/stop.sh"
grep -Fq 'Database and Redis are configured in .env; skipping managed dependency creation.' "$TEST_ROOT/first.log" ||
  fail 'startup did not select configured dependencies from .env'
[[ "$(< "$MIGRATION_COUNT")" == 1 ]] || fail 'first startup did not run migrations exactly once'
grep -Fxq $'production\tfalse\tfalse' "$RUNTIME_ENV_LOG" ||
  fail 'completed production Host did not keep Cookie Secure and administrator MFA opt-in by default'
[[ ! -e "$DOCKER_LOG" ]] || fail 'configured external dependencies unexpectedly invoked Docker'
cmp -s "$TEST_ROOT/completed-before.env" "$PACKAGE_ROOT/.env" ||
  fail 'completed Host startup changed operator .env contents'
[[ "$(stat -Lc '%d:%i' "$PACKAGE_ROOT/.env")" == "$completed_identity" ]] ||
  fail 'completed Host startup replaced the operator .env file'

write_host_environment partial false configured
if "$PACKAGE_ROOT/start.sh" --background > "$TEST_ROOT/partial.log" 2>&1; then
  fail 'startup accepted only one dependency URL'
fi
grep -Fq 'configure DATABASE_URL and REDIS_URL together' "$TEST_ROOT/partial.log" ||
  fail 'startup did not reject partial dependency configuration'
[[ ! -e "$DOCKER_LOG" ]] || fail 'partial dependency configuration unexpectedly invoked Docker'
[[ "$(< "$MIGRATION_COUNT")" == 1 ]] || fail 'failed startup unexpectedly ran migrations'

write_host_environment empty true completed
cp "$PACKAGE_ROOT/.env" "$TEST_ROOT/completed-missing-before.env"
if "$PACKAGE_ROOT/start.sh" --background > "$TEST_ROOT/completed-missing.log" 2>&1; then
  fail 'completed Host silently reprovisioned missing dependencies'
fi
grep -Fq 'completed Host configuration must retain non-empty DATABASE_URL and REDIS_URL' "$TEST_ROOT/completed-missing.log" ||
  fail 'completed Host did not fail closed for missing dependency URLs'
cmp -s "$TEST_ROOT/completed-missing-before.env" "$PACKAGE_ROOT/.env" ||
  fail 'completed Host with missing dependencies changed .env'
[[ ! -e "$DOCKER_LOG" ]] || fail 'completed Host with missing dependencies unexpectedly invoked Docker'

write_host_environment empty false configured
cp "$PACKAGE_ROOT/.env" "$TEST_ROOT/managed-failure-before.env"
managed_failure_identity="$(stat -Lc '%d:%i' "$PACKAGE_ROOT/.env")"
if "$PACKAGE_ROOT/start.sh" --background > "$TEST_ROOT/managed.log" 2>&1; then
  fail 'managed dependency fixture unexpectedly completed with a failing Docker stub'
fi
grep -Fq 'Database and Redis are absent from .env; running first-start dependency initialization.' "$TEST_ROOT/managed.log" ||
  fail 'startup did not select first-start initialization from .env'
grep -Fq 'run -d --name wenzwork-postgres' "$DOCKER_LOG" ||
  fail 'empty dependency configuration did not attempt managed PostgreSQL creation'
[[ "$(< "$MIGRATION_COUNT")" == 1 ]] || fail 'failed managed initialization unexpectedly ran migrations'
cmp -s "$TEST_ROOT/managed-failure-before.env" "$PACKAGE_ROOT/.env" ||
  fail 'failed managed initialization changed .env'
[[ "$(stat -Lc '%d:%i' "$PACKAGE_ROOT/.env")" == "$managed_failure_identity" ]] ||
  fail 'failed managed initialization replaced .env'

rm -f "$DOCKER_LOG"
write_host_environment empty false configured
cp "$PACKAGE_ROOT/.env" "$TEST_ROOT/postgres-only-before.env"
postgres_only_size="$(wc -c < "$TEST_ROOT/postgres-only-before.env" | tr -d '[:space:]')"
postgres_only_identity="$(stat -Lc '%d:%i' "$PACKAGE_ROOT/.env")"
export WENZWORK_TEST_DOCKER_MODE=postgres-only
if "$PACKAGE_ROOT/start.sh" --background > "$TEST_ROOT/postgres-only.log" 2>&1; then
  fail 'managed initialization accepted a Redis container creation failure'
fi
cmp -n "$postgres_only_size" "$TEST_ROOT/postgres-only-before.env" "$PACKAGE_ROOT/.env" >/dev/null ||
  fail 'partial managed initialization did not retain the original .env bytes'
[[ "$(stat -Lc '%d:%i' "$PACKAGE_ROOT/.env")" == "$postgres_only_identity" ]] ||
  fail 'partial managed initialization replaced .env'
[[ "$(grep -c '^DATABASE_URL=' "$PACKAGE_ROOT/.env")" == 1 ]] ||
  fail 'partial managed initialization did not retain the created PostgreSQL credential'
[[ "$(grep -c '^REDIS_URL=' "$PACKAGE_ROOT/.env" || true)" == 0 ]] ||
  fail 'partial managed initialization persisted a Redis credential for a failed container'
[[ "$(< "$MIGRATION_COUNT")" == 1 ]] || fail 'partial managed initialization unexpectedly ran migrations'

rm -f "$DOCKER_LOG"
write_host_environment empty false configured
cp "$PACKAGE_ROOT/.env" "$TEST_ROOT/managed-success-before.env"
managed_success_size="$(wc -c < "$TEST_ROOT/managed-success-before.env" | tr -d '[:space:]')"
managed_success_identity="$(stat -Lc '%d:%i' "$PACKAGE_ROOT/.env")"
export WENZWORK_TEST_DOCKER_MODE=success
"$PACKAGE_ROOT/start.sh" --background > "$TEST_ROOT/managed-success.log"
"$PACKAGE_ROOT/stop.sh"
cmp -n "$managed_success_size" "$TEST_ROOT/managed-success-before.env" "$PACKAGE_ROOT/.env" >/dev/null ||
  fail 'managed initialization did not retain the original .env bytes'
[[ "$(stat -Lc '%d:%i' "$PACKAGE_ROOT/.env")" == "$managed_success_identity" ]] ||
  fail 'managed initialization replaced .env instead of appending values'
[[ "$(grep -c '^DATABASE_URL=' "$PACKAGE_ROOT/.env")" == 1 ]] ||
  fail 'managed initialization did not append exactly one database URL'
[[ "$(grep -c '^REDIS_URL=' "$PACKAGE_ROOT/.env")" == 1 ]] ||
  fail 'managed initialization did not append exactly one Redis URL'
grep -Fq 'ADVANCED_SETTING=retain-me' "$PACKAGE_ROOT/.env" ||
  fail 'managed initialization discarded an advanced operator setting'
[[ "$(< "$MIGRATION_COUNT")" == 2 ]] || fail 'successful managed initialization did not run migrations'

rm -f "$DOCKER_LOG"
export WENZWORK_TEST_DOCKER_MODE=failure
write_host_environment external true completed
printf 'COOKIE_SECURE=true\nADMIN_MFA_REQUIRED=true\n' >> "$PACKAGE_ROOT/.env"
package_set_env_value "$PACKAGE_ROOT/config/package.env" WENZWORK_PACKAGE_VERSION v1.1.0
"$PACKAGE_ROOT/start.sh" --background > "$TEST_ROOT/upgrade.log"
"$PACKAGE_ROOT/stop.sh"
[[ "$(< "$MIGRATION_COUNT")" == 3 ]] || fail 'new-version startup did not run migrations'
grep -Fxq $'production\ttrue\ttrue' "$RUNTIME_ENV_LOG" ||
  fail 'completed Host did not preserve explicit Cookie Secure and administrator MFA opt-ins'
[[ ! -e "$DOCKER_LOG" ]] || fail 'new-version startup attempted to create managed dependencies'

if ! grep -Fq "runtime\\state" "$SCRIPT_DIR/windows/Init.ps1" ||
  ! grep -Fq "'deployed-version'" "$SCRIPT_DIR/windows/Init.ps1"; then
  fail 'Windows initialization does not persist the deployed version'
fi
grep -Fq 'skipping managed PostgreSQL and Redis creation' "$SCRIPT_DIR/windows/Init.ps1" ||
  fail 'Windows initialization does not gate managed dependency creation'

[[ ! -e "$SCRIPT_DIR/unix/init.sh" ]] || fail 'removed Unix init.sh still exists'

printf 'host startup initialization tests passed\n'
