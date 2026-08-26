#!/usr/bin/env bash

set -Eeuo pipefail

REPOSITORY_ROOT="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/wenzwork-upgrade-test.XXXXXX")"
INSTALL_DIR="$TEST_ROOT/install"
PACKAGE_DIR="$TEST_ROOT/package"
FAKE_BIN_DIR="$TEST_ROOT/fake-bin"
ARCHIVE_NAME="wenzwork-server-linux-amd64-v-next.tar.gz"
CHECKSUM_NAME="wenzwork-v-next-SHA256SUMS.txt"
ARCHIVE="$TEST_ROOT/$ARCHIVE_NAME"
CHECKSUM_FILE="$TEST_ROOT/$CHECKSUM_NAME"
RELEASE_METADATA="$TEST_ROOT/latest-release.json"
CALL_LOG="$TEST_ROOT/calls.log"
CURL_ARGS_LOG="$TEST_ROOT/curl-args.log"
ACCESS_TOKEN="github_pat_private_upgrade_test"

cleanup() {
  if [[ -x "$INSTALL_DIR/stop_server.sh" ]]; then
    WENZWORK_HEALTHCHECK_URL=off "$INSTALL_DIR/stop_server.sh" >/dev/null 2>&1 || true
  elif [[ -x "$INSTALL_DIR/start_server.sh" ]]; then
    WENZWORK_HEALTHCHECK_URL=off "$INSTALL_DIR/start_server.sh" stop >/dev/null 2>&1 || true
  fi
  rm -rf -- "$TEST_ROOT"
}

trap cleanup EXIT

mkdir -p \
  "$INSTALL_DIR/bin" "$INSTALL_DIR/migrations" "$INSTALL_DIR/web/assets" \
  "$PACKAGE_DIR/bin" "$PACKAGE_DIR/migrations" "$PACKAGE_DIR/web/assets" \
  "$FAKE_BIN_DIR"

for script in init_server.sh start_server.sh stop_server.sh upgrade_server.sh upgrage_server.sh; do
  cp "$REPOSITORY_ROOT/deploy/$script" "$PACKAGE_DIR/$script"
done
cp "$REPOSITORY_ROOT/deploy/start_server.sh" "$INSTALL_DIR/start_server.sh"
cp "$REPOSITORY_ROOT/deploy/stop_server.sh" "$INSTALL_DIR/stop_server.sh"
cp "$REPOSITORY_ROOT/deploy/upgrade_server.sh" "$INSTALL_DIR/upgrade_server.sh"
cp "$REPOSITORY_ROOT/deploy/upgrage_server.sh" "$INSTALL_DIR/upgrage_server.sh"
cp "$REPOSITORY_ROOT/deploy/Caddyfile" "$PACKAGE_DIR/Caddyfile"
cp "$REPOSITORY_ROOT/.env.example" "$PACKAGE_DIR/.env.example"

cat > "$INSTALL_DIR/init_server.sh" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'init:old\n' >> "$WENZWORK_TEST_CALL_LOG"
EOF

cat > "$INSTALL_DIR/.env" <<EOF
APP_ENV=production
DATABASE_URL=postgres://upgrade-test
WEB_ROOT=web
GITHUB_RELEASE_REPOSITORY=acme/private-wenzwork
GITHUB_ACCESS_TOKEN=$ACCESS_TOKEN
EOF
chmod 600 "$INSTALL_DIR/.env"

cat > "$INSTALL_DIR/bin/wenzwork-api" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'api:old:start\n' >> "$WENZWORK_TEST_CALL_LOG"
trap 'printf "api:old:stop\n" >> "$WENZWORK_TEST_CALL_LOG"; exit 0' TERM INT
while true; do sleep 1; done
EOF

cat > "$INSTALL_DIR/bin/wenzwork-migrate" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'migrate:old:%s\n' "${DATABASE_URL:-}" >> "$WENZWORK_TEST_CALL_LOG"
EOF

cat > "$PACKAGE_DIR/bin/wenzwork-api" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ -n "${GITHUB_ACCESS_TOKEN:-}" ]]; then
  printf 'api:new:token-leaked\n' >> "$WENZWORK_TEST_CALL_LOG"
fi
printf 'api:new:start\n' >> "$WENZWORK_TEST_CALL_LOG"
trap 'printf "api:new:stop\n" >> "$WENZWORK_TEST_CALL_LOG"; exit 0' TERM INT
while true; do sleep 1; done
EOF

cat > "$PACKAGE_DIR/bin/wenzwork-migrate" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${1:-}" == "up" ]]
if [[ -n "${GITHUB_ACCESS_TOKEN:-}" ]]; then
  printf 'migrate:new:token-leaked\n' >> "$WENZWORK_TEST_CALL_LOG"
fi
printf 'migrate:new:%s\n' "${DATABASE_URL:-}" >> "$WENZWORK_TEST_CALL_LOG"
EOF

cat > "$PACKAGE_DIR/bin/wenzwork-admin" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

printf 'old-web\n' > "$INSTALL_DIR/web/index.html"
printf 'obsolete\n' > "$INSTALL_DIR/web/assets/obsolete.js"
printf 'v-old\n' > "$INSTALL_DIR/VERSION"
printf 'new-web\n' > "$PACKAGE_DIR/web/index.html"
printf 'current\n' > "$PACKAGE_DIR/web/assets/current.js"
printf 'migration fixture\n' > "$PACKAGE_DIR/migrations/README"
printf 'v-next\n' > "$PACKAGE_DIR/VERSION"

chmod 0755 \
	"$INSTALL_DIR/init_server.sh" "$INSTALL_DIR/start_server.sh" "$INSTALL_DIR/stop_server.sh" \
  "$INSTALL_DIR/upgrade_server.sh" "$INSTALL_DIR/upgrage_server.sh" \
  "$INSTALL_DIR/bin/wenzwork-api" "$INSTALL_DIR/bin/wenzwork-migrate" \
  "$PACKAGE_DIR/init_server.sh" "$PACKAGE_DIR/start_server.sh" \
  "$PACKAGE_DIR/stop_server.sh" "$PACKAGE_DIR/upgrade_server.sh" \
  "$PACKAGE_DIR/upgrage_server.sh"
# Simulate an archive extracted on a noexec filesystem or one whose executable
# mode was not preserved. The upgrader must validate the files without running
# them from staging, then restore executable permissions after installation.
chmod 0644 \
  "$PACKAGE_DIR/bin/wenzwork-api" \
  "$PACKAGE_DIR/bin/wenzwork-admin" \
  "$PACKAGE_DIR/bin/wenzwork-migrate"

tar -C "$PACKAGE_DIR" -czf "$ARCHIVE" .
archive_hash="$(sha256sum "$ARCHIVE" | awk '{print $1}')"
archive_hash="${archive_hash#\\}"
# Cover checksum manifests created by local Windows release builds.
printf '%s  %s\r\n' "$archive_hash" "$ARCHIVE_NAME" > "$CHECKSUM_FILE"

# Keep this response compact: GitHub JSON formatting is not an API contract,
# and the production upgrader must not rely on one field per line.
cat > "$RELEASE_METADATA" <<'EOF'
{"url":"https://api.github.com/repos/acme/private-wenzwork/releases/99","tag_name":"v-next","assets":[{"url":"https://api.github.com/repos/acme/private-wenzwork/releases/assets/101","id":101,"name":"wenzwork-server-linux-amd64-v-next.tar.gz"},{"url":"https://api.github.com/repos/acme/private-wenzwork/releases/assets/102","id":102,"name":"wenzwork-v-next-SHA256SUMS.txt"}]}
EOF

cat > "$FAKE_BIN_DIR/curl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

output=""
config=""
url=""
if [[ -n "${GITHUB_ACCESS_TOKEN:-}" ]]; then
  printf 'curl:token-env-leaked\n' >> "$WENZWORK_TEST_CALL_LOG"
fi
while (( $# > 0 )); do
  printf 'arg:%s\n' "$1" >> "$WENZWORK_TEST_CURL_ARGS_LOG"
  case "$1" in
    --output | --config | --header | --retry | --retry-delay | --connect-timeout | --max-time | --proto | --proto-redir)
      option="$1"
      value="${2:-}"
      printf 'arg:%s\n' "$value" >> "$WENZWORK_TEST_CURL_ARGS_LOG"
      [[ "$option" != "--output" ]] || output="$value"
      [[ "$option" != "--config" ]] || config="$value"
      shift 2
      ;;
    --fail | --location | --silent | --show-error)
      shift
      ;;
    https://*)
      url="$1"
      shift
      ;;
    *)
      shift
      ;;
  esac
done

[[ -n "$output" && -n "$url" ]]
if [[ -n "$config" ]]; then
  grep -Fxq "header = \"Authorization: Bearer $WENZWORK_TEST_EXPECTED_TOKEN\"" "$config"
  if [[ "$(uname -s)" != MINGW* && "$(uname -s)" != MSYS* ]]; then
    [[ "$(stat -c '%a' "$config")" == "600" ]]
  fi
  printf 'auth-config:yes\n' >> "$WENZWORK_TEST_CALL_LOG"
fi

case "$url" in
  https://api.github.com/repos/acme/private-wenzwork/releases/latest)
    cp "$WENZWORK_TEST_RELEASE_METADATA" "$output"
    ;;
  https://api.github.com/repos/acme/private-wenzwork/releases/assets/101)
    cp "$WENZWORK_TEST_RELEASE_ARCHIVE" "$output"
    ;;
  https://api.github.com/repos/acme/private-wenzwork/releases/assets/102)
    cp "$WENZWORK_TEST_RELEASE_CHECKSUM" "$output"
    ;;
  *)
    printf 'unexpected URL: %s\n' "$url" >&2
    exit 22
    ;;
esac
EOF
chmod 0755 "$FAKE_BIN_DIR/curl"

export PATH="$FAKE_BIN_DIR:$PATH"
export WENZWORK_TEST_CALL_LOG="$CALL_LOG"
export WENZWORK_TEST_CURL_ARGS_LOG="$CURL_ARGS_LOG"
export WENZWORK_TEST_EXPECTED_TOKEN="$ACCESS_TOKEN"
export WENZWORK_TEST_RELEASE_METADATA="$RELEASE_METADATA"
export WENZWORK_TEST_RELEASE_ARCHIVE="$ARCHIVE"
export WENZWORK_TEST_RELEASE_CHECKSUM="$CHECKSUM_FILE"

WENZWORK_HEALTHCHECK_URL=off "$INSTALL_DIR/start_server.sh" start >/dev/null
WENZWORK_HEALTHCHECK_URL=off "$INSTALL_DIR/upgrade_server.sh"

[[ "$(cat "$INSTALL_DIR/VERSION")" == "v-next" ]]
[[ "$(cat "$INSTALL_DIR/web/index.html")" == "new-web" ]]
[[ -f "$INSTALL_DIR/web/assets/current.js" ]]
[[ ! -e "$INSTALL_DIR/web/assets/obsolete.js" ]]
[[ -x "$INSTALL_DIR/bin/wenzwork-api" ]]
[[ -x "$INSTALL_DIR/bin/wenzwork-admin" ]]
[[ -x "$INSTALL_DIR/bin/wenzwork-migrate" ]]
grep -Fxq "GITHUB_ACCESS_TOKEN=$ACCESS_TOKEN" "$INSTALL_DIR/.env"
grep -Fxq 'api:old:start' "$CALL_LOG"
grep -Fxq 'api:old:stop' "$CALL_LOG"
grep -Fxq 'migrate:new:postgres://upgrade-test' "$CALL_LOG"
grep -Fxq 'api:new:start' "$CALL_LOG"
if grep -Fq 'token-leaked' "$CALL_LOG"; then
  printf 'GITHUB_ACCESS_TOKEN leaked into a runtime process\n' >&2
  exit 1
fi
if grep -Fq "$ACCESS_TOKEN" "$CURL_ARGS_LOG"; then
  printf 'GITHUB_ACCESS_TOKEN leaked into curl process arguments\n' >&2
  exit 1
fi
[[ "$(grep -Fc 'auth-config:yes' "$CALL_LOG")" -eq 3 ]]

backup_dir="$(find "$INSTALL_DIR/backups" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
[[ -n "$backup_dir" ]]
[[ "$(cat "$backup_dir/VERSION")" == "v-old" ]]
[[ "$(cat "$backup_dir/web/index.html")" == "old-web" ]]

# The misspelled compatibility entrypoint should still resolve the latest
# metadata and return without reinstalling an already-current version.
WENZWORK_HEALTHCHECK_URL=off "$INSTALL_DIR/upgrage_server.sh" >/dev/null
WENZWORK_HEALTHCHECK_URL=off "$INSTALL_DIR/start_server.sh" status >/dev/null
WENZWORK_HEALTHCHECK_URL=off "$INSTALL_DIR/stop_server.sh" >/dev/null
grep -Fxq 'api:new:stop' "$CALL_LOG"
if WENZWORK_HEALTHCHECK_URL=off "$INSTALL_DIR/start_server.sh" status >/dev/null 2>&1; then
  printf 'stop_server.sh left the service running\n' >&2
  exit 1
fi

printf 'upgrade_server.sh GitHub token and stop_server.sh integration test passed\n'
