#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'filesystem_boundary_integration_test: %s\n' "$*" >&2
  exit 1
}

[[ ${WENZWORK_AGENT_FILESYSTEM_BOUNDARY_EPHEMERAL_ROOT:-} == I_UNDERSTAND_THIS_USES_PRIVATE_TEST_MOUNTS ]] ||
  fail 'explicit private-mount confirmation is required'
[[ ${EUID:-$(id -u)} -eq 0 ]] || fail 'the filesystem boundary integration test must run as root'
[[ $(uname -s) == Linux ]] || fail 'the filesystem boundary integration test requires Linux'
if [[ ! -f /.dockerenv ]]; then
  [[ ${GITHUB_ACTIONS:-} == true && ${RUNNER_ENVIRONMENT:-} == github-hosted ]] ||
    fail 'run only in a disposable container or a GitHub-hosted runner'
fi
for command in bash find grep mktemp mount rm stat umount unshare; do
  command -v "$command" >/dev/null 2>&1 || fail "required command is missing: $command"
done

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
test_dir=$(mktemp -d /tmp/wenzwork-device-agent-filesystem.XXXXXX)
# shellcheck disable=SC2329 # Invoked indirectly by trap.
cleanup() {
  [[ ${test_dir:-} == /tmp/wenzwork-device-agent-filesystem.?????? ]] || return 1
  rm -rf -- "$test_dir"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$test_dir/mounted-root" "$test_dir/tree/mounted" "$test_dir/tree/state"
export WENZWORK_AGENT_BOUNDARY_COMMON="$script_dir/lib/common.sh"
export WENZWORK_AGENT_BOUNDARY_TEST_DIR="$test_dir"

unshare --mount --fork bash <<'BOUNDARY_TEST'
set -euo pipefail

fail() {
  printf 'filesystem_boundary_integration_test: %s\n' "$*" >&2
  exit 1
}

mount --make-rprivate /
mounted_root="$WENZWORK_AGENT_BOUNDARY_TEST_DIR/mounted-root"
tree_root="$WENZWORK_AGENT_BOUNDARY_TEST_DIR/tree"
nested_mount="$tree_root/mounted"
mount -t tmpfs -o size=1m,nodev,nosuid,noexec tmpfs "$mounted_root"
mount -t tmpfs -o size=1m,nodev,nosuid,noexec tmpfs "$nested_mount"
# shellcheck disable=SC2329 # Invoked indirectly by trap.
cleanup_mounts() {
  umount "$nested_mount" 2>/dev/null || true
  umount "$mounted_root" 2>/dev/null || true
}
trap cleanup_mounts EXIT HUP INT TERM

# shellcheck source=lib/common.sh
source "$WENZWORK_AGENT_BOUNDARY_COMMON"
[[ $(agent_file_device "$mounted_root") != "$(agent_file_device "$(dirname -- "$mounted_root")")" ]] ||
  fail 'mounted data-root fixture is not on a distinct filesystem'
[[ $(agent_file_device "$nested_mount") != "$(agent_file_device "$tree_root")" ]] ||
  fail 'nested mount fixture is not on a distinct filesystem'

if (agent_assert_atomic_restore_layout "$mounted_root") 2>"$WENZWORK_AGENT_BOUNDARY_TEST_DIR/root.error"; then
  fail 'a filesystem-mounted data root was accepted'
fi
if (agent_assert_regular_data_tree "$tree_root" 'mounted tree') 2>"$WENZWORK_AGENT_BOUNDARY_TEST_DIR/tree.error"; then
  fail 'a nested filesystem mount was accepted in the backup data tree'
fi
grep -q 'must not be a filesystem mount point' "$WENZWORK_AGENT_BOUNDARY_TEST_DIR/root.error" ||
  fail 'mounted data-root rejection did not use the stable diagnostic'
grep -q 'contains a filesystem mount point' "$WENZWORK_AGENT_BOUNDARY_TEST_DIR/tree.error" ||
  fail 'nested mount rejection did not use the stable diagnostic'
BOUNDARY_TEST

printf 'filesystem_boundary_integration_test: PASS\n'
