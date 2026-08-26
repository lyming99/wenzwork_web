#!/usr/bin/env bash

set -Eeuo pipefail

[[ $(uname -s) == Linux ]] || {
  printf 'portable upgrade source test is Linux-only\n'
  exit 0
}

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/wenzwork-upgrade-source-test.XXXXXX")"
PACKAGE_ROOT="$TEST_ROOT/current"
FAKE_BIN="$TEST_ROOT/bin"
CURL_LOG="$TEST_ROOT/curl.log"

cleanup() {
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT HUP INT TERM

fail() {
  printf 'portable upgrade source test failed: %s\n' "$*" >&2
  exit 1
}

# shellcheck source=unix/lib/common.sh
source "$SCRIPT_DIR/unix/lib/common.sh"

write_package() {
  local root="$1" version="$2" extension=''
  rm -rf -- "$root"
  mkdir -p "$root/bin" "$root/config" "$root/runtime/lib" "$root/logs" \
    "$root/workspace" "$root/cache" "$root/runtime"
  cp "$SCRIPT_DIR/unix/upgrade.sh" "$root/upgrade.sh"
  cp "$SCRIPT_DIR/unix/lib/common.sh" "$root/runtime/lib/common.sh"
  for name in start.sh stop.sh; do
    printf '#!/usr/bin/env bash\nexit 0\n' > "$root/$name"
  done
  cat > "$root/.env" <<'EOF'
GITHUB_RELEASE_REPOSITORY=example/wenzwork
GITHUB_ACCESS_TOKEN=
EOF
  cat > "$root/config/package.env" <<EOF
WENZWORK_PACKAGE_COMPONENT=device-agent
WENZWORK_PACKAGE_PLATFORM=linux
WENZWORK_PACKAGE_ARCHITECTURE=amd64
WENZWORK_PACKAGE_VERSION=$version
WENZWORK_PACKAGE_ASSET_BASENAME=wenzwork-device-agent-deployment
WENZWORK_PACKAGE_CHECKSUM_ASSET=DEPLOYMENT-SHA256SUMS
WENZWORK_GITHUB_REPOSITORY=example/wenzwork
EOF
  cat > "$root/config/device-agent.env.example" <<'EOF'
WENZWORK_CONTROL_URL=https://wenzwork.com
WENZWORK_DEVICE_ACCESS_KEY=device_replace_with_a_43_character_urlsafe_access_key
EOF
  printf '#!/usr/bin/env bash\nexit 0\n' > "$root/bin/wenzwork-device-agent$extension"
  printf '%s\n' "$version" > "$root/VERSION"
  printf '{}\n' > "$root/PACKAGE-MANIFEST.json"
  chmod 0755 "$root"/*.sh "$root/bin"/*
  chmod 0600 "$root/.env"
}

build_release_fixture() {
  local version="$1" fixture_root archive_name
  fixture_root="$TEST_ROOT/fixture-$version"
  archive_name="wenzwork-device-agent-deployment-$version-linux-amd64.tar.gz"
  write_package "$fixture_root" "$version"
  rm -f -- "$fixture_root/.env"
  tar -czf "$TEST_ROOT/$archive_name" -C "$fixture_root" .
  WENZWORK_TEST_ARCHIVE="$TEST_ROOT/$archive_name"
  WENZWORK_TEST_ARCHIVE_NAME="$archive_name"
  WENZWORK_TEST_ARCHIVE_SHA256="$(package_sha256 "$WENZWORK_TEST_ARCHIVE")"
  WENZWORK_TEST_CHECKSUMS="$TEST_ROOT/DEPLOYMENT-SHA256SUMS-$version"
  printf '%s  %s\n' "$WENZWORK_TEST_ARCHIVE_SHA256" "$archive_name" > "$WENZWORK_TEST_CHECKSUMS"
  WENZWORK_TEST_OFFICIAL_JSON="$TEST_ROOT/official-$version.json"
  cat > "$WENZWORK_TEST_OFFICIAL_JSON" <<EOF
{"id":"00000000-0000-0000-0000-000000000001","project":"web","version":"$version","channel":"stable","title":"fixture","summary":"","releaseNotes":"","publishedAt":"2026-08-22T00:00:00Z","assets":[{"id":"00000000-0000-0000-0000-000000000002","platform":"linux","architecture":"x64","fileName":"$archive_name","fileSizeBytes":1,"sha256":"$WENZWORK_TEST_ARCHIVE_SHA256","signatureStatus":"valid","downloadUrl":"/download/$archive_name"}]}
EOF
  export WENZWORK_TEST_ARCHIVE WENZWORK_TEST_ARCHIVE_NAME WENZWORK_TEST_ARCHIVE_SHA256
  export WENZWORK_TEST_CHECKSUMS WENZWORK_TEST_OFFICIAL_JSON
}

mkdir -p "$FAKE_BIN"
cat > "$FAKE_BIN/curl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
destination=''
url=''
while (( $# > 0 )); do
  case "$1" in
    --output) destination="$2"; shift 2 ;;
    *) url="$1"; shift ;;
  esac
done
printf '%s\n' "$url" >> "$WENZWORK_TEST_CURL_LOG"
case "$url" in
  https://work.wenzflow.com/api/*)
    [[ "$WENZWORK_TEST_MODE" == primary ]] || exit 22
    cp "$WENZWORK_TEST_OFFICIAL_JSON" "$destination"
    ;;
  https://work.wenzflow.com/download/*)
    [[ "$WENZWORK_TEST_MODE" == primary ]] || exit 22
    cp "$WENZWORK_TEST_ARCHIVE" "$destination"
    ;;
  https://wenzwork.com/api/*)
    [[ "$WENZWORK_TEST_MODE" == secondary ]] || exit 22
    cp "$WENZWORK_TEST_OFFICIAL_JSON" "$destination"
    ;;
  https://wenzwork.com/download/*)
    [[ "$WENZWORK_TEST_MODE" == secondary ]] || exit 22
    cp "$WENZWORK_TEST_ARCHIVE" "$destination"
    ;;
  https://github.com/*/DEPLOYMENT-SHA256SUMS)
    [[ "$WENZWORK_TEST_MODE" == github ]] || exit 22
    cp "$WENZWORK_TEST_CHECKSUMS" "$destination"
    ;;
  https://github.com/*/"$WENZWORK_TEST_ARCHIVE_NAME")
    [[ "$WENZWORK_TEST_MODE" == github ]] || exit 22
    cp "$WENZWORK_TEST_ARCHIVE" "$destination"
    ;;
  *) exit 22 ;;
esac
EOF
chmod 0755 "$FAKE_BIN/curl"
export WENZWORK_TEST_CURL_LOG="$CURL_LOG"
export PATH="$FAKE_BIN:$PATH"

write_package "$PACKAGE_ROOT" v1.0.0
cp "$PACKAGE_ROOT/.env" "$TEST_ROOT/original.env"
build_release_fixture v1.1.0
export WENZWORK_TEST_MODE=primary
"$PACKAGE_ROOT/upgrade.sh" > "$TEST_ROOT/primary.log" 2>&1
[[ "$(package_metadata_value "$PACKAGE_ROOT" WENZWORK_PACKAGE_VERSION)" == v1.1.0 ]] ||
  fail 'work.wenzflow.com fixture was not installed'
cmp -s "$TEST_ROOT/original.env" "$PACKAGE_ROOT/.env" ||
  fail 'upgrade replaced the installed .env with archive contents'
grep -Fq 'Downloaded wenzwork-device-agent-deployment-v1.1.0-linux-amd64.tar.gz from https://work.wenzflow.com.' "$TEST_ROOT/primary.log" ||
  fail 'work.wenzflow.com success was not reported'
grep -Fq 'Progress 100%: Upgrade completed: v1.0.0 -> v1.1.0.' "$TEST_ROOT/primary.log" ||
  fail 'upgrade completion progress was not reported'
! grep -Fq 'wenzwork.com/' "$CURL_LOG" || fail 'wenzwork.com was contacted after work.wenzflow.com succeeded'
! grep -Fq 'github.com/' "$CURL_LOG" || fail 'GitHub was contacted after work.wenzflow.com succeeded'

build_release_fixture v1.2.0
export WENZWORK_TEST_MODE=secondary
: > "$CURL_LOG"
"$PACKAGE_ROOT/upgrade.sh" > "$TEST_ROOT/secondary.log" 2>&1
[[ "$(package_metadata_value "$PACKAGE_ROOT" WENZWORK_PACKAGE_VERSION)" == v1.2.0 ]] ||
  fail 'wenzwork.com fallback fixture was not installed'
cmp -s "$TEST_ROOT/original.env" "$PACKAGE_ROOT/.env" ||
  fail 'wenzwork.com fallback did not preserve the installed .env'
grep -Fq 'No matching upgrade package is available from https://work.wenzflow.com; trying the next source.' "$TEST_ROOT/secondary.log" ||
  fail 'work.wenzflow.com fallback was not reported'
grep -Fq 'Downloaded wenzwork-device-agent-deployment-v1.2.0-linux-amd64.tar.gz from https://wenzwork.com.' "$TEST_ROOT/secondary.log" ||
  fail 'wenzwork.com success was not reported'
work_line="$(grep -n -m1 'work.wenzflow.com/api/' "$CURL_LOG" | cut -d: -f1)"
wenzwork_line="$(grep -n -m1 'wenzwork.com/api/' "$CURL_LOG" | cut -d: -f1)"
[[ -n "$work_line" && -n "$wenzwork_line" && "$work_line" -lt "$wenzwork_line" ]] ||
  fail 'website source priority was not work.wenzflow.com then wenzwork.com'
! grep -Fq 'github.com/' "$CURL_LOG" || fail 'GitHub was contacted after wenzwork.com succeeded'

build_release_fixture v1.3.0
export WENZWORK_TEST_MODE=github
: > "$CURL_LOG"
"$PACKAGE_ROOT/upgrade.sh" > "$TEST_ROOT/github.log" 2>&1
[[ "$(package_metadata_value "$PACKAGE_ROOT" WENZWORK_PACKAGE_VERSION)" == v1.3.0 ]] ||
  fail 'GitHub fallback fixture was not installed'
cmp -s "$TEST_ROOT/original.env" "$PACKAGE_ROOT/.env" ||
  fail 'GitHub fallback did not preserve the installed .env'
grep -Fq 'Progress 30%: Trying release source github.com.' "$TEST_ROOT/github.log" ||
  fail 'GitHub fallback progress was not reported'
grep -Fq 'Downloaded wenzwork-device-agent-deployment-v1.3.0-linux-amd64.tar.gz from the public GitHub Release page.' "$TEST_ROOT/github.log" ||
  fail 'public GitHub fallback success was not reported'
work_line="$(grep -n -m1 'work.wenzflow.com/api/' "$CURL_LOG" | cut -d: -f1)"
wenzwork_line="$(grep -n -m1 'wenzwork.com/api/' "$CURL_LOG" | cut -d: -f1)"
github_line="$(grep -n -m1 'github.com/' "$CURL_LOG" | cut -d: -f1)"
[[ -n "$work_line" && -n "$wenzwork_line" && -n "$github_line" &&
    "$work_line" -lt "$wenzwork_line" && "$wenzwork_line" -lt "$github_line" ]] ||
  fail 'download source priority was not work.wenzflow.com, wenzwork.com, then GitHub'

build_release_fixture v1.4.0
export WENZWORK_TEST_MODE=missing
: > "$CURL_LOG"
if "$PACKAGE_ROOT/upgrade.sh" > "$TEST_ROOT/missing.log" 2>&1; then
  fail 'upgrade unexpectedly succeeded when every source was missing'
fi
[[ "$(package_metadata_value "$PACKAGE_ROOT" WENZWORK_PACKAGE_VERSION)" == v1.3.0 ]] ||
  fail 'failed source lookup changed the installed version'
grep -Fq 'ERROR: upgrade package was not found at work.wenzflow.com, wenzwork.com, or github.com' "$TEST_ROOT/missing.log" ||
  fail 'three-source failure was not reported'

grep -Fq -- '--progress-bar' "$SCRIPT_DIR/unix/lib/common.sh" ||
  fail 'curl transfer progress is disabled'
grep -Fq -- '--progress=bar:force' "$SCRIPT_DIR/unix/lib/common.sh" ||
  fail 'wget transfer progress is disabled'

printf 'portable upgrade source priority tests passed\n'
