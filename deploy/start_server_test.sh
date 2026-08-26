#!/usr/bin/env bash

set -Eeuo pipefail

REPOSITORY_ROOT="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/wenzwork-start-test.XXXXXX")"
INSTALL_DIR="$TEST_ROOT/install"
PACKAGE_DIR="$TEST_ROOT/package"
ARCHIVE="$TEST_ROOT/wenzwork-server-test.tar.gz"
CALL_LOG="$TEST_ROOT/calls.log"

cleanup() {
  if [[ -x "$INSTALL_DIR/start_server.sh" ]]; then
    WENZWORK_HEALTHCHECK_URL=off "$INSTALL_DIR/start_server.sh" stop >/dev/null 2>&1 || true
  fi
  rm -rf -- "$TEST_ROOT"
}

trap cleanup EXIT

mkdir -p \
  "$INSTALL_DIR/bin" "$INSTALL_DIR/migrations" "$INSTALL_DIR/web/assets" \
  "$INSTALL_DIR/cache/releases" \
  "$PACKAGE_DIR/bin" "$PACKAGE_DIR/migrations" "$PACKAGE_DIR/web/assets"

cp "$REPOSITORY_ROOT/deploy/start_server.sh" "$INSTALL_DIR/start_server.sh"
cp "$REPOSITORY_ROOT/deploy/start_server.sh" "$PACKAGE_DIR/start_server.sh"
cp "$REPOSITORY_ROOT/deploy/init_server.sh" "$PACKAGE_DIR/init_server.sh"
cp "$REPOSITORY_ROOT/deploy/stop_server.sh" "$PACKAGE_DIR/stop_server.sh"
cp "$REPOSITORY_ROOT/deploy/upgrade_server.sh" "$PACKAGE_DIR/upgrade_server.sh"
cp "$REPOSITORY_ROOT/deploy/upgrage_server.sh" "$PACKAGE_DIR/upgrage_server.sh"
cp "$REPOSITORY_ROOT/deploy/Caddyfile" "$PACKAGE_DIR/Caddyfile"
cp "$REPOSITORY_ROOT/.env.example" "$PACKAGE_DIR/.env.example"

cat > "$INSTALL_DIR/.env" <<'EOF'
APP_ENV=production
DATABASE_URL=postgres://test
WEB_ROOT=web
EOF

cat > "$PACKAGE_DIR/bin/wenzwork-api" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${COOKIE_SECURE:-}" == false ]]
[[ "${ADMIN_MFA_REQUIRED:-}" == false ]]
printf 'api:start\n' >> "$WENZWORK_TEST_CALL_LOG"
trap 'exit 0' TERM INT
while true; do
  sleep 1
done
EOF

cat > "$PACKAGE_DIR/bin/wenzwork-migrate" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${1:-}" == "up" ]]
printf 'migrate:%s\n' "${DATABASE_URL:-}" >> "$WENZWORK_TEST_CALL_LOG"
EOF

cat > "$PACKAGE_DIR/bin/wenzwork-admin" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

cat > "$INSTALL_DIR/bin/wenzwork-api" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF

cat > "$INSTALL_DIR/bin/wenzwork-migrate" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF

printf 'old-web\n' > "$INSTALL_DIR/web/index.html"
printf 'obsolete\n' > "$INSTALL_DIR/web/assets/obsolete.js"
printf 'cached-installer\n' > "$INSTALL_DIR/cache/releases/cached-installer"
printf 'v-old\n' > "$INSTALL_DIR/VERSION"

printf 'new-web\n' > "$PACKAGE_DIR/web/index.html"
printf 'current\n' > "$PACKAGE_DIR/web/assets/current.js"
printf 'migration fixture\n' > "$PACKAGE_DIR/migrations/README"
printf 'v-next\n' > "$PACKAGE_DIR/VERSION"

chmod 0755 \
  "$INSTALL_DIR/start_server.sh" \
  "$INSTALL_DIR/bin/wenzwork-api" \
  "$INSTALL_DIR/bin/wenzwork-migrate" \
  "$PACKAGE_DIR/init_server.sh" \
  "$PACKAGE_DIR/start_server.sh" \
  "$PACKAGE_DIR/stop_server.sh" \
  "$PACKAGE_DIR/upgrade_server.sh" \
  "$PACKAGE_DIR/upgrage_server.sh" \
  "$PACKAGE_DIR/bin/wenzwork-api" \
  "$PACKAGE_DIR/bin/wenzwork-admin" \
  "$PACKAGE_DIR/bin/wenzwork-migrate"

tar -C "$PACKAGE_DIR" -czf "$ARCHIVE" .

export WENZWORK_TEST_CALL_LOG="$CALL_LOG"
WENZWORK_HEALTHCHECK_URL=off "$INSTALL_DIR/start_server.sh" upgrade "$ARCHIVE"

[[ "$(cat "$INSTALL_DIR/VERSION")" == "v-next" ]]
[[ "$(cat "$INSTALL_DIR/web/index.html")" == "new-web" ]]
[[ "$(cat "$INSTALL_DIR/web/assets/current.js")" == "current" ]]
[[ ! -e "$INSTALL_DIR/web/assets/obsolete.js" ]]
[[ "$(cat "$INSTALL_DIR/cache/releases/cached-installer")" == "cached-installer" ]]
grep -Fxq 'migrate:postgres://test' "$CALL_LOG"
grep -Fxq 'api:start' "$CALL_LOG"
WENZWORK_HEALTHCHECK_URL=off "$INSTALL_DIR/start_server.sh" status >/dev/null

backup_dir="$(find "$INSTALL_DIR/backups" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
[[ -n "$backup_dir" ]]
[[ "$(cat "$backup_dir/VERSION")" == "v-old" ]]
[[ "$(cat "$backup_dir/web/index.html")" == "old-web" ]]
[[ -f "$backup_dir/web/assets/obsolete.js" ]]

WENZWORK_HEALTHCHECK_URL=off "$INSTALL_DIR/start_server.sh" stop >/dev/null
printf 'start_server.sh upgrade integration test passed\n'
