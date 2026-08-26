#!/usr/bin/env bash

set -Eeuo pipefail

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
REPOSITORY_ROOT="$(CDPATH='' cd -- "$SCRIPT_DIR/../.." && pwd -P)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/wenzwork-unix-package-test.XXXXXX")"
PACKAGE_ROOT="$TEST_ROOT/wenzwork-device-agent"

cleanup() {
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT HUP INT TERM

fail() {
  printf 'unix package compatibility test failed: %s\n' "$*" >&2
  exit 1
}

# shellcheck source=unix/lib/common.sh
source "$SCRIPT_DIR/unix/lib/common.sh"

unset WENZWORK_PACKAGE_COMPATIBILITY_VALUE
package_export_default WENZWORK_PACKAGE_COMPATIBILITY_VALUE default
[[ "$WENZWORK_PACKAGE_COMPATIBILITY_VALUE" == default ]] ||
  fail 'an unset variable did not receive its default'
WENZWORK_PACKAGE_COMPATIBILITY_VALUE=preserved
package_export_default WENZWORK_PACKAGE_COMPATIBILITY_VALUE replacement
[[ "$WENZWORK_PACKAGE_COMPATIBILITY_VALUE" == preserved ]] ||
  fail 'a non-empty variable was overwritten'
WENZWORK_PACKAGE_COMPATIBILITY_VALUE=''
package_export_default WENZWORK_PACKAGE_COMPATIBILITY_VALUE default
[[ "$WENZWORK_PACKAGE_COMPATIBILITY_VALUE" == default ]] ||
  fail 'an empty variable did not receive its default'

case "$(uname -s 2>/dev/null || true)" in
  Linux) host_platform=linux ;;
  Darwin) host_platform=darwin ;;
  MINGW* | MSYS* | CYGWIN*) host_platform=windows ;;
  *) fail "unsupported test platform: $(uname -s 2>/dev/null || printf unknown)" ;;
esac
case "$(uname -m 2>/dev/null || true)" in
  x86_64 | amd64) host_architecture=amd64 ;;
  arm64 | aarch64) host_architecture=arm64 ;;
  *) fail "unsupported test architecture: $(uname -m 2>/dev/null || printf unknown)" ;;
esac

mkdir -p "$PACKAGE_ROOT/bin" "$PACKAGE_ROOT/config" "$PACKAGE_ROOT/runtime/lib" \
  "$PACKAGE_ROOT/workspace" "$PACKAGE_ROOT/cache"
cp "$REPOSITORY_ROOT/deploy/package/unix/start.sh" "$PACKAGE_ROOT/start.sh"
cp "$REPOSITORY_ROOT/deploy/package/unix/stop.sh" "$PACKAGE_ROOT/stop.sh"
cp "$REPOSITORY_ROOT/deploy/package/unix/upgrade.sh" "$PACKAGE_ROOT/upgrade.sh"
cp "$REPOSITORY_ROOT/deploy/package/unix/lib/common.sh" "$PACKAGE_ROOT/runtime/lib/common.sh"
printf '%s\n' \
  'WENZWORK_PACKAGE_COMPONENT=device-agent' \
  "WENZWORK_PACKAGE_PLATFORM=$host_platform" \
  "WENZWORK_PACKAGE_ARCHITECTURE=$host_architecture" \
  'WENZWORK_PACKAGE_VERSION=compatibility-test' \
  'WENZWORK_PACKAGE_ASSET_BASENAME=wenzwork-device-agent' \
  'WENZWORK_PACKAGE_CHECKSUM_ASSET=DEPLOYMENT-SHA256SUMS' \
  > "$PACKAGE_ROOT/config/package.env"
: > "$PACKAGE_ROOT/config/device-agent.env.example"
: > "$PACKAGE_ROOT/.env"
printf '%s\n' compatibility-test > "$PACKAGE_ROOT/VERSION"
printf '%s\n' '{}' > "$PACKAGE_ROOT/PACKAGE-MANIFEST.json"

extension=''
[[ "$host_platform" != windows ]] || extension='.exe'
executable="$PACKAGE_ROOT/bin/wenzwork-device-agent$extension"
# Keep the fixture's expansions for its own runtime.
# shellcheck disable=SC2016
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -Eeuo pipefail' \
  'root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"' \
  'printf "%s\n" "$*" > "$root/runtime/state/test-invocation"' \
  > "$executable"
chmod 0755 "$executable" "$PACKAGE_ROOT/start.sh" "$PACKAGE_ROOT/stop.sh" "$PACKAGE_ROOT/upgrade.sh"

"$BASH" "$PACKAGE_ROOT/start.sh" --foreground compatibility-sentinel
[[ "$(cat "$PACKAGE_ROOT/runtime/state/test-invocation")" == 'serve compatibility-sentinel' ]] ||
  fail 'Device Agent start arguments did not reach the executable'

printf 'Unix package compatibility test passed with Bash %s on %s/%s.\n' \
  "$BASH_VERSION" "$host_platform" "$host_architecture"
