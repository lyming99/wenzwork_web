#!/usr/bin/env bash

set -Eeuo pipefail

REPOSITORY_ROOT="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/wenzwork-init-test.XXXXXX")"
FIXTURE_DIR="$TEST_ROOT/server"
CALL_LOG="$TEST_ROOT/calls.log"
FAKE_ADMIN_STATE="$TEST_ROOT/admin-state"

cleanup() {
  rm -rf -- "$TEST_ROOT"
}

trap cleanup EXIT

mkdir -p "$FIXTURE_DIR/bin" "$FIXTURE_DIR/migrations"
cp "$REPOSITORY_ROOT/deploy/init_server.sh" "$FIXTURE_DIR/init_server.sh"
expected_host_settings="$(printf '%s\n' \
  SYSTEM_ADMIN_EMAIL SYSTEM_ADMIN_PASSWORD SYSTEM_SETUP_COMPLETED | sort)"
actual_host_settings="$(awk -F= '/^[A-Za-z_][A-Za-z0-9_]*=/ {print $1}' "$REPOSITORY_ROOT/.env.example" | sort)"
[[ "$actual_host_settings" == "$expected_host_settings" ]] || {
  printf 'Host .env.example contains settings outside the core configuration\n' >&2
  exit 1
}

cp "$REPOSITORY_ROOT/.env.example" "$FIXTURE_DIR/.env.example"
sed -i \
  -e 's|^SYSTEM_ADMIN_EMAIL=.*|SYSTEM_ADMIN_EMAIL=admin@example.test|' \
  -e 's|^SYSTEM_ADMIN_PASSWORD=.*|SYSTEM_ADMIN_PASSWORD=test1234|' \
  "$FIXTURE_DIR/.env.example"
cat >> "$FIXTURE_DIR/.env.example" <<'EOF'
DATABASE_URL=postgres://test
REDIS_URL=redis://test:6379/0
SYSTEM_ADMIN_DISPLAY_NAME=WenzWork Test Administrator
S3_ENDPOINT=http://s3.example.test
S3_REGION=us-east-1
S3_BUCKET=wenzwork-test
S3_ACCESS_KEY_ID=test-access-key
S3_SECRET_ACCESS_KEY=test-secret-key
S3_ADDRESSING_STYLE=path
S3_SESSION_TOKEN=
GITHUB_ACCESS_TOKEN=github_pat_init_must_not_inherit
EOF

cat > "$FIXTURE_DIR/bin/wenzwork-migrate" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${1:-}" == "up" ]]
[[ -z "${GITHUB_ACCESS_TOKEN:-}" ]]
printf 'migrate:%s\n' "${DATABASE_URL:-}" >> "$WENZWORK_TEST_CALL_LOG"
EOF

cat > "$FIXTURE_DIR/bin/wenzwork-admin" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ -z "${GITHUB_ACCESS_TOKEN:-}" ]]
[[ "${APP_ENV:-}" == "development" ]]
[[ "${PUBLIC_BASE_URL:-}" == "http://localhost:8080" ]]
[[ "${COOKIE_SECURE:-}" == "false" ]]
[[ "${ADMIN_MFA_REQUIRED:-}" == "false" ]]
[[ "${REMOTE_MVP_ENABLED:-}" == "true" ]]
[[ "${HOST_SECRETS_FILE:-}" == "$WENZWORK_TEST_SERVER_ROOT/cache/host-secrets/application.env" ]]
if [[ "${1:-}" == "smtp" ]]; then
  [[ "${2:-}" == "test" && "${3:-}" == "--env-file" && -f "${4:-}" && -z "${5:-}" ]]
  [[ "${SYSTEM_ADMIN_EMAIL:-}" == "admin@example.test" ]]
  printf 'smtp:%s\n' "$SYSTEM_ADMIN_EMAIL" >> "$WENZWORK_TEST_CALL_LOG"
  exit 0
fi
if [[ "${1:-}" == "s3" ]]; then
  [[ "${2:-}" == "test" && "${3:-}" == "--env-file" && -f "${4:-}" && -z "${5:-}" ]]
  [[ "${S3_ENDPOINT:-}" == "http://s3.example.test" ]]
  [[ "${S3_REGION:-}" == "us-east-1" ]]
  [[ "${S3_BUCKET:-}" == "wenzwork-test" ]]
  [[ "${S3_ACCESS_KEY_ID:-}" == "test-access-key" ]]
  [[ "${S3_SECRET_ACCESS_KEY:-}" == "test-secret-key" ]]
  [[ "${S3_ADDRESSING_STYLE:-}" == "path" ]]
  printf 's3:%s\n' "$S3_BUCKET" >> "$WENZWORK_TEST_CALL_LOG"
  exit 0
fi
[[ "${1:-}" == "bootstrap" ]]
if [[ "${2:-}" == "status" ]]; then
  if [[ -f "$WENZWORK_TEST_ADMIN_STATE" ]]; then
    printf 'initialized\tadmin@example.test\n'
  else
    printf 'uninitialized\n'
  fi
  exit 0
fi
[[ -z "${2:-}" ]]
[[ "${BOOTSTRAP_ADMIN_EMAIL:-}" == "admin@example.test" ]]
[[ "${BOOTSTRAP_ADMIN_PASSWORD:-}" == "test1234" ]]
[[ "${BOOTSTRAP_ADMIN_DISPLAY_NAME:-}" == "WenzWork Test Administrator" ]]
touch "$WENZWORK_TEST_ADMIN_STATE"
printf 'admin:%s\n' "$BOOTSTRAP_ADMIN_EMAIL" >> "$WENZWORK_TEST_CALL_LOG"
EOF

chmod 0755 \
  "$FIXTURE_DIR/init_server.sh" \
  "$FIXTURE_DIR/bin/wenzwork-admin" \
  "$FIXTURE_DIR/bin/wenzwork-migrate"

export WENZWORK_TEST_CALL_LOG="$CALL_LOG"
export WENZWORK_TEST_ADMIN_STATE="$FAKE_ADMIN_STATE"
export WENZWORK_TEST_SERVER_ROOT="$FIXTURE_DIR"
"$FIXTURE_DIR/init_server.sh" | tee "$TEST_ROOT/first-run.log"

[[ -f "$FIXTURE_DIR/.env" ]]
if [[ "$(uname -s)" != MINGW* && "$(uname -s)" != MSYS* ]]; then
  [[ "$(stat -c '%a' "$FIXTURE_DIR/.env")" == "600" ]]
fi
! grep -Eq '^(APP_ENV|PUBLIC_BASE_URL|REMOTE_MVP_ENABLED|MFA_ENCRYPTION_KEY|REDEMPTION_CODE_HMAC_KEY)=' "$FIXTURE_DIR/.env"
grep -Fxq 'migrate:postgres://test' "$CALL_LOG"
grep -Fxq 'admin:admin@example.test' "$CALL_LOG"
[[ "$(grep -Fc 'admin:admin@example.test' "$CALL_LOG")" == "1" ]]

sed -i 's/^SYSTEM_ADMIN_PASSWORD=.*/SYSTEM_ADMIN_PASSWORD=/' "$FIXTURE_DIR/.env"
"$FIXTURE_DIR/init_server.sh" | tee "$TEST_ROOT/second-run.log"
grep -Fxq 'SYSTEM_ADMIN_PASSWORD=' "$FIXTURE_DIR/.env"
[[ "$(grep -Fc 'admin:admin@example.test' "$CALL_LOG")" == "1" ]]

legacy_dir="$TEST_ROOT/legacy-server"
mkdir -p "$legacy_dir/migrations"
cp -a "$FIXTURE_DIR/bin" "$legacy_dir/bin"
cp "$FIXTURE_DIR/init_server.sh" "$legacy_dir/init_server.sh"
grep -v '^SYSTEM_ADMIN_' "$FIXTURE_DIR/.env.example" > "$legacy_dir/.env.example"
if WENZWORK_TEST_ADMIN_STATE="$TEST_ROOT/legacy-admin-state" \
  "$legacy_dir/init_server.sh" > "$TEST_ROOT/legacy-run.log" 2>&1; then
  printf 'legacy environment unexpectedly initialized without administrator settings\n' >&2
  exit 1
fi
grep -Fxq 'SYSTEM_ADMIN_EMAIL=' "$legacy_dir/.env"
grep -Fxq 'SYSTEM_ADMIN_PASSWORD=' "$legacy_dir/.env"
grep -Fxq 'SYSTEM_ADMIN_DISPLAY_NAME=WenzWork Administrator' "$legacy_dir/.env"
grep -Fq 'Set SYSTEM_ADMIN_EMAIL and SYSTEM_ADMIN_PASSWORD' "$TEST_ROOT/legacy-run.log"

printf 'init_server.sh integration test passed\n'
