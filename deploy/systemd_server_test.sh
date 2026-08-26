#!/usr/bin/env bash

set -Eeuo pipefail

REPOSITORY_ROOT="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/wenzwork-systemd-test.XXXXXX")"
INSTALL_DIR="$TEST_ROOT/install"
PACKAGE_DIR="$TEST_ROOT/package"
FAKE_BIN_DIR="$TEST_ROOT/fake-bin"
UNIT_DIR="$TEST_ROOT/systemd"
STATE_FILE="$TEST_ROOT/service.state"
CALL_LOG="$TEST_ROOT/calls.log"
RUNTIME_LOG="$TEST_ROOT/runtime.log"
ARCHIVE="$TEST_ROOT/wenzwork-server-systemd-test.tar.gz"
ACCESS_TOKEN="deployment-token-must-not-leak"

cleanup() {
  rm -rf -- "$TEST_ROOT"
}

trap cleanup EXIT

mkdir -p \
  "$INSTALL_DIR/bin" "$INSTALL_DIR/migrations" "$INSTALL_DIR/web" \
  "$PACKAGE_DIR/bin" "$PACKAGE_DIR/migrations" "$PACKAGE_DIR/web" \
  "$FAKE_BIN_DIR" "$UNIT_DIR"

for script in init_server.sh start_server.sh stop_server.sh upgrade_server.sh upgrage_server.sh; do
  cp "$REPOSITORY_ROOT/deploy/$script" "$INSTALL_DIR/$script"
  cp "$REPOSITORY_ROOT/deploy/$script" "$PACKAGE_DIR/$script"
done

for path in "$INSTALL_DIR/init_server.sh" "$PACKAGE_DIR/init_server.sh"; do
  cat > "$path" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'init\n' >> "$WENZWORK_TEST_SYSTEMD_CALL_LOG"
EOF
done
cp "$REPOSITORY_ROOT/deploy/Caddyfile" "$PACKAGE_DIR/Caddyfile"
cp "$REPOSITORY_ROOT/.env.example" "$PACKAGE_DIR/.env.example"

cat > "$INSTALL_DIR/.env" <<EOF
APP_ENV=production
DATABASE_URL=postgres://systemd-test
WEB_ROOT=web
GITHUB_ACCESS_TOKEN=$ACCESS_TOKEN
GOMEMLIMIT=192MiB
WENZWORK_MEMORY_HIGH=256M
WENZWORK_MEMORY_MAX=320M
EOF
chmod 600 "$INSTALL_DIR/.env"

cat > "$INSTALL_DIR/bin/wenzwork-api" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ -z "${GITHUB_ACCESS_TOKEN:-}" ]] || printf 'api:old:token-leaked\n' >> "$WENZWORK_TEST_RUNTIME_LOG"
printf 'api:old:foreground:%s\n' "${GOMEMLIMIT:-unset}" >> "$WENZWORK_TEST_RUNTIME_LOG"
EOF

cat > "$INSTALL_DIR/bin/wenzwork-migrate" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${1:-}" == "up" ]]
[[ -z "${GITHUB_ACCESS_TOKEN:-}" ]] || printf 'migrate:old:token-leaked\n' >> "$WENZWORK_TEST_RUNTIME_LOG"
printf 'migrate:old:%s\n' "${DATABASE_URL:-unset}" >> "$WENZWORK_TEST_RUNTIME_LOG"
EOF

cat > "$PACKAGE_DIR/bin/wenzwork-api" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ -z "${GITHUB_ACCESS_TOKEN:-}" ]] || printf 'api:new:token-leaked\n' >> "$WENZWORK_TEST_RUNTIME_LOG"
printf 'api:new:foreground:%s\n' "${GOMEMLIMIT:-unset}" >> "$WENZWORK_TEST_RUNTIME_LOG"
EOF

cat > "$PACKAGE_DIR/bin/wenzwork-migrate" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${1:-}" == "up" ]]
[[ -z "${GITHUB_ACCESS_TOKEN:-}" ]] || printf 'migrate:new:token-leaked\n' >> "$WENZWORK_TEST_RUNTIME_LOG"
printf 'migrate:new:%s\n' "${DATABASE_URL:-unset}" >> "$WENZWORK_TEST_RUNTIME_LOG"
EOF

cat > "$PACKAGE_DIR/bin/wenzwork-admin" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

printf 'old-web\n' > "$INSTALL_DIR/web/index.html"
printf 'new-web\n' > "$PACKAGE_DIR/web/index.html"
printf 'migration fixture\n' > "$PACKAGE_DIR/migrations/README"
printf 'v-old\n' > "$INSTALL_DIR/VERSION"
printf 'v-next\n' > "$PACKAGE_DIR/VERSION"

chmod 0755 \
  "$INSTALL_DIR/init_server.sh" "$INSTALL_DIR/start_server.sh" \
  "$INSTALL_DIR/stop_server.sh" "$INSTALL_DIR/upgrade_server.sh" \
  "$INSTALL_DIR/upgrage_server.sh" "$INSTALL_DIR/bin/wenzwork-api" \
  "$INSTALL_DIR/bin/wenzwork-migrate" "$PACKAGE_DIR/init_server.sh" \
  "$PACKAGE_DIR/start_server.sh" "$PACKAGE_DIR/stop_server.sh" \
  "$PACKAGE_DIR/upgrade_server.sh" "$PACKAGE_DIR/upgrage_server.sh" \
  "$PACKAGE_DIR/bin/wenzwork-api" "$PACKAGE_DIR/bin/wenzwork-admin" \
  "$PACKAGE_DIR/bin/wenzwork-migrate"

cat > "$FAKE_BIN_DIR/systemctl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

command="${1:-}"
shift || true
printf 'systemctl:%s:%s\n' "$command" "$*" >> "$WENZWORK_TEST_SYSTEMD_CALL_LOG"

case "$command" in
  show)
    property=""
    for argument in "$@"; do
      case "$argument" in
        --property=*) property="${argument#--property=}" ;;
      esac
    done
    case "$property" in
      LoadState)
        if [[ -f "$WENZWORK_TEST_SYSTEMD_UNIT_DIR/wenzwork-api.service" ]]; then
          printf 'loaded\n'
        else
          printf 'not-found\n'
        fi
        ;;
      MainPID) printf '4242\n' ;;
      NRestarts) printf '2\n' ;;
      *) printf '\n' ;;
    esac
    ;;
  is-active)
    state="$(cat "$WENZWORK_TEST_SYSTEMD_STATE" 2>/dev/null || printf 'inactive')"
    if [[ "$*" != *"--quiet"* ]]; then
      printf '%s\n' "$state"
    fi
    [[ "$state" == "active" ]]
    ;;
  start | restart)
    printf 'active\n' > "$WENZWORK_TEST_SYSTEMD_STATE"
    ;;
  stop)
    printf 'inactive\n' > "$WENZWORK_TEST_SYSTEMD_STATE"
    ;;
  daemon-reload | enable)
    ;;
  *)
    printf 'unexpected systemctl command: %s %s\n' "$command" "$*" >&2
    exit 2
    ;;
esac
EOF
chmod 0755 "$FAKE_BIN_DIR/systemctl"

export WENZWORK_TEST_SYSTEMD_CALL_LOG="$CALL_LOG"
export WENZWORK_TEST_SYSTEMD_STATE="$STATE_FILE"
export WENZWORK_TEST_SYSTEMD_UNIT_DIR="$UNIT_DIR"
export WENZWORK_TEST_RUNTIME_LOG="$RUNTIME_LOG"
export WENZWORK_SYSTEMCTL="$FAKE_BIN_DIR/systemctl"
export WENZWORK_SYSTEMD_UNIT_DIR="$UNIT_DIR"
export WENZWORK_ALLOW_NON_ROOT_SYSTEMD_INSTALL=1
export WENZWORK_ALLOW_NON_LINUX_SYSTEMD_INSTALL=1
export WENZWORK_HEALTHCHECK_URL=off

if WENZWORK_PROCESS_MANAGER=systemd "$INSTALL_DIR/start_server.sh" start >/dev/null 2>&1; then
  printf 'an explicitly requested missing systemd unit unexpectedly started\n' >&2
  exit 1
fi
if WENZWORK_PROCESS_MANAGER=invalid "$INSTALL_DIR/start_server.sh" start >/dev/null 2>&1; then
  printf 'an invalid process manager unexpectedly started\n' >&2
  exit 1
fi
if [[ -e "$RUNTIME_LOG" || -e "$INSTALL_DIR/run/wenzwork-api.pid" ]]; then
  printf 'process-manager validation unexpectedly fell back to standalone mode\n' >&2
  exit 1
fi

"$INSTALL_DIR/start_server.sh" install-systemd >/dev/null

UNIT_FILE="$UNIT_DIR/wenzwork-api.service"
[[ -f "$UNIT_FILE" ]]
grep -Fxq 'Restart=always' "$UNIT_FILE"
grep -Fxq 'RestartSec=3s' "$UNIT_FILE"
grep -Fxq 'MemoryAccounting=true' "$UNIT_FILE"
grep -Fxq 'MemoryHigh=256M' "$UNIT_FILE"
grep -Fxq 'MemoryMax=320M' "$UNIT_FILE"
grep -Fq 'ExecStartPre=' "$UNIT_FILE"
grep -Fq 'start_server.sh" migrate' "$UNIT_FILE"
grep -Fq 'start_server.sh" run' "$UNIT_FILE"
if grep -Fq 'EnvironmentFile=' "$UNIT_FILE"; then
  printf 'systemd unit must not expose the deployment environment file directly\n' >&2
  exit 1
fi

grep -Fq 'systemctl:daemon-reload:' "$CALL_LOG"
grep -Fq 'systemctl:enable:' "$CALL_LOG"
grep -Fq 'systemctl:start:' "$CALL_LOG"
"$INSTALL_DIR/start_server.sh" status >/dev/null
"$INSTALL_DIR/start_server.sh" restart >/dev/null
"$INSTALL_DIR/start_server.sh" stop >/dev/null
"$INSTALL_DIR/start_server.sh" start >/dev/null
[[ "$(cat "$STATE_FILE")" == "active" ]]
[[ ! -e "$INSTALL_DIR/run/wenzwork-api.pid" ]]
grep -Fxq 'init' "$CALL_LOG"

"$INSTALL_DIR/start_server.sh" migrate >/dev/null
"$INSTALL_DIR/start_server.sh" run >/dev/null
grep -Fxq 'migrate:old:postgres://systemd-test' "$RUNTIME_LOG"
grep -Fxq 'api:old:foreground:192MiB' "$RUNTIME_LOG"
if grep -Fq 'token-leaked' "$RUNTIME_LOG"; then
  printf 'GITHUB_ACCESS_TOKEN leaked into a systemd runtime command\n' >&2
  exit 1
fi

tar -C "$PACKAGE_DIR" -czf "$ARCHIVE" .
: > "$CALL_LOG"
"$INSTALL_DIR/upgrade_server.sh" "$ARCHIVE" >/dev/null

[[ "$(cat "$INSTALL_DIR/VERSION")" == "v-next" ]]
[[ "$(cat "$INSTALL_DIR/web/index.html")" == "new-web" ]]
grep -Fq 'systemctl:stop:' "$CALL_LOG"
grep -Fq 'systemctl:start:' "$CALL_LOG"
[[ "$(grep -Fc 'systemctl:stop:' "$CALL_LOG")" -eq 1 ]]
[[ "$(grep -Fc 'systemctl:start:' "$CALL_LOG")" -eq 1 ]]
[[ ! -e "$INSTALL_DIR/run/wenzwork-api.pid" ]]

"$INSTALL_DIR/start_server.sh" migrate >/dev/null
"$INSTALL_DIR/start_server.sh" run >/dev/null
grep -Fxq 'migrate:new:postgres://systemd-test' "$RUNTIME_LOG"
grep -Fxq 'api:new:foreground:192MiB' "$RUNTIME_LOG"
if grep -Fq 'token-leaked' "$RUNTIME_LOG"; then
  printf 'GITHUB_ACCESS_TOKEN leaked after a systemd-managed upgrade\n' >&2
  exit 1
fi

printf 'systemd lifecycle, memory limits, token isolation, and upgrade test passed\n'
