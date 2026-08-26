#!/usr/bin/env bash

set -Eeuo pipefail

[[ $(uname -s) == Linux ]] || {
  printf 'portable Relay startup test is Linux-only\n'
  exit 0
}

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/wenzwork-relay-start-test.XXXXXX")"
PACKAGE_ROOT="$TEST_ROOT/package"

cleanup() {
  if [[ -f "$PACKAGE_ROOT/runtime/pids/wenzwork.pid" ]]; then
    pid="$(tr -d '[:space:]' < "$PACKAGE_ROOT/runtime/pids/wenzwork.pid")"
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] && kill "$pid" >/dev/null 2>&1 || true
  fi
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT HUP INT TERM

fail() {
  printf 'portable Relay startup test failed: %s\n' "$*" >&2
  exit 1
}

# shellcheck source=unix/lib/common.sh
source "$SCRIPT_DIR/unix/lib/common.sh"
platform="$(package_host_platform)"
architecture="$(package_host_architecture)"

mkdir -p "$PACKAGE_ROOT/bin" "$PACKAGE_ROOT/config" "$PACKAGE_ROOT/runtime/lib" \
  "$PACKAGE_ROOT/workspace" "$PACKAGE_ROOT/cache"
cp "$SCRIPT_DIR/unix/start.sh" "$PACKAGE_ROOT/start.sh"
cp "$SCRIPT_DIR/unix/stop.sh" "$PACKAGE_ROOT/stop.sh"
cp "$SCRIPT_DIR/unix/lib/common.sh" "$PACKAGE_ROOT/runtime/lib/common.sh"
printf '#!/usr/bin/env bash\nexit 0\n' > "$PACKAGE_ROOT/upgrade.sh"
printf 'v1.0.0\n' > "$PACKAGE_ROOT/VERSION"
printf '{}\n' > "$PACKAGE_ROOT/PACKAGE-MANIFEST.json"

cat > "$PACKAGE_ROOT/config/package.env" <<EOF
WENZWORK_PACKAGE_COMPONENT=relay
WENZWORK_PACKAGE_PLATFORM=$platform
WENZWORK_PACKAGE_ARCHITECTURE=$architecture
WENZWORK_PACKAGE_VERSION=v1.0.0
WENZWORK_PACKAGE_ASSET_BASENAME=wenzwork-relay-deployment
WENZWORK_PACKAGE_CHECKSUM_ASSET=DEPLOYMENT-SHA256SUMS
WENZWORK_GITHUB_REPOSITORY=example/wenzwork
EOF
printf 'RELAY_ACCESS_KEY=relay_test\n' > "$PACKAGE_ROOT/config/relay.env.example"
cp "$PACKAGE_ROOT/config/relay.env.example" "$PACKAGE_ROOT/.env"

cat > "$PACKAGE_ROOT/bin/wenzwork-relay-server" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
trap 'exit 0' TERM INT
while true; do
  sleep 1
done
EOF

output=''
if ! output="$("$PACKAGE_ROOT/start.sh" --background 2>&1)"; then
  fail "start.sh returned an error: $output"
fi
grep -Fq 'relay started as PID' <<< "$output" ||
  fail "start.sh did not report a successful Relay start: $output"

pid="$(tr -d '[:space:]' < "$PACKAGE_ROOT/runtime/pids/wenzwork.pid")"
[[ "$pid" =~ ^[1-9][0-9]*$ ]] || fail 'start.sh did not persist a valid PID'
kill -0 "$pid" 2>/dev/null || fail 'Relay process is not running after start.sh returned'

printf 'portable Relay startup test passed\n'
