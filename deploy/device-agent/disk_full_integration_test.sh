#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'disk_full_integration_test: %s\n' "$*" >&2
  exit 1
}

[[ ${WENZWORK_AGENT_DISK_FULL_EPHEMERAL_ROOT:-} == I_UNDERSTAND_THIS_FILLS_A_PRIVATE_TEST_FILESYSTEM ]] ||
  fail 'explicit private-filesystem confirmation is required'
[[ ${EUID:-$(id -u)} -eq 0 ]] || fail 'the disk-full integration test must run as root'
[[ $(uname -s) == Linux ]] || fail 'the disk-full integration test requires Linux'
if [[ ! -f /.dockerenv ]]; then
  [[ ${GITHUB_ACTIONS:-} == true && ${RUNNER_ENVIRONMENT:-} == github-hosted ]] ||
    fail 'run only in a disposable container or a GitHub-hosted runner'
fi
for command in bash mktemp mount rm umount unshare; do
  command -v "$command" >/dev/null 2>&1 || fail "required command is missing: $command"
done

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
repo_root=$(cd -- "$script_dir/../.." && pwd -P)
test_dir=$(mktemp -d /tmp/wenzwork-device-agent-disk-full.XXXXXX)
# shellcheck disable=SC2329 # Invoked indirectly by trap.
cleanup() {
  [[ ${test_dir:-} == /tmp/wenzwork-device-agent-disk-full.?????? ]] || return 1
  rm -rf -- "$test_dir"
}
trap cleanup EXIT HUP INT TERM

test_binary=${WENZWORK_AGENT_DISK_FULL_TEST_BINARY:-}
if [[ -z $test_binary ]]; then
  command -v go >/dev/null 2>&1 || fail 'go is required when no prebuilt test binary is supplied'
  test_binary="$test_dir/device-agent.test"
  CGO_ENABLED=0 go -C "$repo_root/server" test -c -o "$test_binary" ./cmd/device-agent
fi
[[ $test_binary == /* && -x $test_binary && -f $test_binary && ! -L $test_binary ]] ||
  fail 'the Device Agent test binary must be an executable absolute regular file'

volume_root="$test_dir/volume"
mkdir -p "$volume_root"
export WENZWORK_AGENT_DISK_FULL_TEST_BINARY="$test_binary"
export WENZWORK_AGENT_DISK_FULL_TEST_ROOT="$volume_root"
unshare --mount --fork bash <<'DISK_FULL_TEST'
set -euo pipefail
mount --make-rprivate /
mount -t tmpfs -o size=32m,nodev,nosuid,noexec tmpfs "$WENZWORK_AGENT_DISK_FULL_TEST_ROOT"
# shellcheck disable=SC2329 # Invoked indirectly by trap.
cleanup_mount() { umount "$WENZWORK_AGENT_DISK_FULL_TEST_ROOT" 2>/dev/null || true; }
trap cleanup_mount EXIT HUP INT TERM
WENZWORK_AGENT_DISK_FULL_EPHEMERAL_ROOT=I_UNDERSTAND_THIS_FILLS_A_PRIVATE_TEST_FILESYSTEM \
  WENZWORK_AGENT_DISK_FULL_TEST_ROOT="$WENZWORK_AGENT_DISK_FULL_TEST_ROOT" \
  "$WENZWORK_AGENT_DISK_FULL_TEST_BINARY" \
  '-test.run=^TestAgentStateRealDiskFullFailsAtomicallyAndRecovers$' '-test.v=true' '-test.count=1'
DISK_FULL_TEST

printf 'disk_full_integration_test: PASS\n'
