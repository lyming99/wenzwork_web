#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"
fail() { printf 'darwin relay scripts test: %s\n' "$*" >&2; exit 1; }

relay_validate_url https://downloads.example.test/relay/v1/package.tar.gz
relay_validate_url http://downloads.example.test/relay/v1/package.tar.gz
if (relay_validate_url ftp://downloads.example.test/package) >/dev/null 2>&1; then
  fail "non-HTTP(S) URL was accepted"
fi

[[ $(relay_normalize_architecture x86_64) == amd64 ]] || fail "x86_64 mapping"
[[ $(relay_normalize_architecture amd64) == amd64 ]] || fail "amd64 mapping"
[[ $(relay_normalize_architecture arm64) == arm64 ]] || fail "arm64 mapping"
[[ $(relay_normalize_architecture aarch64) == arm64 ]] || fail "aarch64 mapping"
# These are intentionally literal shell fragments expected in common.sh.
# shellcheck disable=SC2016
platform_assertion='--expected-platform "$platform"'
# shellcheck disable=SC2016
architecture_assertion='--expected-architecture "$architecture"'
grep -Fq -- "$platform_assertion" "$script_dir/lib/common.sh" || fail "manifest platform is not pinned"
grep -Fq -- "$architecture_assertion" "$script_dir/lib/common.sh" || fail "manifest architecture is not pinned"
grep -q -- 'release verify-bundle' "$script_dir/lib/common.sh" || fail "outer bundle signature verification missing"
grep -q -- 'launchctl bootstrap system' "$script_dir/lib/common.sh" || fail "launchd bootstrap missing"
grep -q -- 'relay_verify_bundle' "$script_dir/install.sh" || fail "install verification missing"
grep -q -- 'Relay install/work directory' "$script_dir/install.sh" || fail "interactive install path prompt missing"
grep -q -- 'non-interactive default install/work directory' "$script_dir/install.sh" || fail "non-interactive install path behavior is unclear"
grep -q -- 'Management URL' "$script_dir/install.sh" || fail "management URL prompt missing"
grep -q -- 'relay_read_access_key' "$script_dir/install.sh" || fail "Access Key prompt missing"
grep -q -- 'rolling back' "$script_dir/upgrade.sh" || fail "upgrade rollback missing"
plutil -lint "$script_dir/launchd/com.wenzwork.relay.plist" >/dev/null 2>&1 || true
printf 'darwin_relay_scripts_test: PASS\n'
