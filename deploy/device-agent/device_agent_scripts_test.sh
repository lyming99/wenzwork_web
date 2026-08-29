#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

fail() { printf 'device_agent_scripts_test: %s\n' "$*" >&2; exit 1; }
expect_failure() {
  if "$@" >/dev/null 2>&1; then
    fail "command unexpectedly succeeded: $*"
  fi
}

agent_validate_url https://control.example.test
agent_validate_url http://localhost:8080
agent_validate_url http://127.0.0.1:8080/api
agent_validate_url http://control.example.test:8080/api
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_validate_url ftp://control.example.test"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_validate_url https://user:pass@control.example.test"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_validate_url https://control.example.test?token=secret"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_validate_url https:///missing-host"
agent_validate_download_url 'https://downloads.example.test/agent.tar.gz?temporary=token'
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_validate_download_url 'https://user:pass@downloads.example.test/agent.tar.gz'"

[[ $(agent_validate_install_root /opt/wenzwork-device-agent/) == /opt/wenzwork-device-agent ]] ||
  fail "install root was not normalized"
agent_validate_data_root /var/lib/wenzwork-device-agent >/dev/null
agent_validate_config_root '/Library/Application Support/WenzWork/DeviceAgent' >/dev/null
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_validate_install_root /"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_validate_install_root /opt"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_validate_install_root /opt/../etc/device-agent"

[[ $(agent_normalize_architecture x86_64) == amd64 ]] || fail "x86_64 architecture was not normalized"
[[ $(agent_normalize_architecture aarch64) == arm64 ]] || fail "aarch64 architecture was not normalized"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_normalize_architecture i686"

for command in openssl sha256sum tar; do
  command -v "$command" >/dev/null 2>&1 || fail "$command is required for this test"
done
test_dir=$(mktemp -d)
cleanup() { [[ -n ${test_dir:-} && $test_dir == /tmp/* ]] && rm -rf -- "$test_dir"; }
trap cleanup EXIT

mkdir -p "$test_dir/real-parent"
if ln -s "$test_dir/real-parent" "$test_dir/link-parent" 2>/dev/null && [[ -L $test_dir/link-parent ]]; then
  expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_validate_install_root '$test_dir/link-parent/agent'"
fi

mkdir -p "$test_dir/fake-bin" "$test_dir/release/bin"
cat > "$test_dir/fake-bin/uname" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
  -s) printf 'Linux\n' ;;
  -m) printf '%s\n' "${AGENT_TEST_UNAME_ARCH:?}" ;;
  *) exit 2 ;;
esac
EOF
cat > "$test_dir/release/bin/relayctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" > "${AGENT_VERIFY_ARGS_FILE:?}"
EOF
chmod 0755 "$test_dir/fake-bin/uname" "$test_dir/release/bin/relayctl"
for architecture_case in 'x86_64:amd64' 'aarch64:arm64'; do
  host_arch=${architecture_case%%:*}
  manifest_arch=${architecture_case##*:}
  verify_args="$test_dir/verify-$manifest_arch.args"
  AGENT_TEST_UNAME_ARCH=$host_arch AGENT_VERIFY_ARGS_FILE=$verify_args \
    PATH="$test_dir/fake-bin:$PATH" agent_verify_release_tree "$test_dir/release" v1.2.3
  grep -q -- '--expected-platform linux' "$verify_args" || fail "manifest platform was not pinned"
  grep -q -- "--expected-architecture $manifest_arch" "$verify_args" || fail "manifest architecture was not pinned"
done

printf 'signed Device Agent test payload\n' > "$test_dir/payload.bin"
(cd "$test_dir" && sha256sum payload.bin > SHA256SUMS)
openssl genpkey -algorithm ED25519 -out "$test_dir/signing.key" >/dev/null 2>&1
openssl pkey -in "$test_dir/signing.key" -pubout -out "$test_dir/signing.pub" >/dev/null 2>&1
openssl pkeyutl -sign -rawin -inkey "$test_dir/signing.key" -in "$test_dir/SHA256SUMS" -out "$test_dir/SHA256SUMS.sig"
AGENT_TEST_UNAME_ARCH=x86_64 PATH="$test_dir/fake-bin:$PATH" \
  agent_verify_bundle "$test_dir/payload.bin" "$test_dir/SHA256SUMS" "$test_dir/SHA256SUMS.sig" "$test_dir/signing.pub"
agent_verify_same_public_key "$test_dir/signing.pub" "$test_dir/signing.pub"
printf 'not the same key\n' > "$test_dir/other.pub"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_verify_same_public_key '$test_dir/signing.pub' '$test_dir/other.pub'"
printf 'tampered\n' >> "$test_dir/payload.bin"
expect_failure env AGENT_TEST_UNAME_ARCH=x86_64 PATH="$test_dir/fake-bin:$PATH" bash -c \
  "source '$script_dir/lib/common.sh'; agent_verify_bundle '$test_dir/payload.bin' '$test_dir/SHA256SUMS' '$test_dir/SHA256SUMS.sig' '$test_dir/signing.pub'"

mkdir "$test_dir/archive-source"
printf 'safe\n' > "$test_dir/archive-source/file.txt"
tar -C "$test_dir/archive-source" -czf "$test_dir/safe.tar.gz" file.txt
agent_extract_bundle "$test_dir/safe.tar.gz" "$test_dir/extracted"
[[ $(<"$test_dir/extracted/file.txt") == safe ]] || fail "safe archive did not extract"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_maximum_uncompressed_archive_bytes=4; agent_extract_bundle '$test_dir/safe.tar.gz' '$test_dir/oversized-output'"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_maximum_archive_entries=0; agent_extract_bundle '$test_dir/safe.tar.gz' '$test_dir/entry-limit-output'"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_maximum_archive_bytes=1; agent_extract_bundle '$test_dir/safe.tar.gz' '$test_dir/archive-limit-output'"
tar -C "$test_dir/archive-source" -czf "$test_dir/duplicate.tar.gz" file.txt file.txt
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_extract_bundle '$test_dir/duplicate.tar.gz' '$test_dir/duplicate-output'"
tar -C "$test_dir/archive-source" --transform='s|^|../|' -czf "$test_dir/unsafe.tar.gz" file.txt
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_extract_bundle '$test_dir/unsafe.tar.gz' '$test_dir/unsafe-output'"
tar -C "$test_dir/archive-source" --transform='s|file.txt|dir\file.txt|' -czf "$test_dir/backslash.tar.gz" file.txt
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_extract_bundle '$test_dir/backslash.tar.gz' '$test_dir/backslash-output'"
if ln -s file.txt "$test_dir/archive-source/linked.txt" 2>/dev/null && [[ -L $test_dir/archive-source/linked.txt ]]; then
  tar -C "$test_dir/archive-source" -czf "$test_dir/symlink.tar.gz" linked.txt
  expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_extract_bundle '$test_dir/symlink.tar.gz' '$test_dir/symlink-output'"
fi

data_root="$test_dir/data"
config_root="$test_dir/config"
backup_root="$test_dir/backups"
state_path="$data_root/state/agent-state.json"
mkdir -p "$data_root/state" "$config_root"
printf 'identity\n' > "$state_path"
printf 'business\n' > "$state_path.business.sqlite"
agent_assert_atomic_restore_layout "$data_root"
mkdir -p "$data_root/state/mounted-fixture"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_file_device() { if [[ \$1 == '$data_root/state/mounted-fixture' ]]; then printf 2; else printf 1; fi; }; agent_assert_regular_data_tree '$data_root' 'test data'"
rmdir -- "$data_root/state/mounted-fixture"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_file_device() { if [[ \$1 == '$data_root' ]]; then printf 2; else printf 1; fi; }; agent_assert_atomic_restore_layout '$data_root'"
if ln -s "$test_dir/payload.bin" "$data_root/state/unsafe-link" 2>/dev/null && [[ -L $data_root/state/unsafe-link ]]; then
  expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_assert_regular_data_tree '$data_root' 'test data'"
  rm -f -- "$data_root/state/unsafe-link"
fi
if ln "$state_path.business.sqlite" "$data_root/state/unsafe-hardlink" 2>/dev/null; then
  expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_assert_regular_data_tree '$data_root' 'test data'"
  rm -f -- "$data_root/state/unsafe-hardlink"
fi
access_key="device_$(printf 'k%.0s' {1..43})"
cat > "$config_root/agent.env" <<EOF
WENZWORK_CONTROL_URL=http://control.example.test:8080
WENZWORK_DEVICE_ACCESS_KEY=$access_key
WENZWORK_DEVICE_DIRECT_ACCESS_KEY=$access_key
WENZWORK_DEVICE_STATE_FILE=$state_path
WENZWORK_DEVICE_WORKSPACE=$data_root/workspace
WENZWORK_AGENT_SECRET_STORE=file
EOF
agent_validate_env_file "$config_root/agent.env" "$state_path"
invalid_direct_env="$test_dir/invalid-direct.env"
sed 's/^WENZWORK_DEVICE_DIRECT_ACCESS_KEY=.*/WENZWORK_DEVICE_DIRECT_ACCESS_KEY=invalid/' "$config_root/agent.env" > "$invalid_direct_env"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_validate_env_file '$invalid_direct_env' '$state_path'"
duplicate_direct_env="$test_dir/duplicate-direct.env"
cp "$config_root/agent.env" "$duplicate_direct_env"
printf 'WENZWORK_DEVICE_DIRECT_ACCESS_KEY=%s\n' "$access_key" >> "$duplicate_direct_env"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_validate_env_file '$duplicate_direct_env' '$state_path'"
backup_dir=$(agent_create_backup "$data_root" "$config_root/agent.env" "$backup_root" v1)
printf 'migrated\n' > "$state_path.business.sqlite"
printf 'changed\n' >> "$config_root/agent.env"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_restore_backup '$backup_dir' '$data_root' '$config_root/agent.env' '$(id -u):$(id -g)' '$test_dir/not-the-backup-root'"
if [[ $(uname -s) == Linux ]]; then
  dd if=/dev/zero of="$backup_dir/data/state/write-limit-fixture.bin" bs=4096 count=2 status=none
  expect_failure bash -c "ulimit -f 1; source '$script_dir/lib/common.sh'; agent_restore_backup '$backup_dir' '$data_root' '$config_root/agent.env' '$(id -u):$(id -g)' '$backup_root'"
  [[ $(<"$state_path.business.sqlite") == migrated ]] || fail "a failed restore modified the active business database"
  grep -q '^changed$' "$config_root/agent.env" || fail "a failed restore modified the active environment"
  rm -f -- "$backup_dir/data/state/write-limit-fixture.bin"
fi
agent_restore_backup "$backup_dir" "$data_root" "$config_root/agent.env" "$(id -u):$(id -g)" "$backup_root"
[[ $(<"$state_path.business.sqlite") == business ]] || fail "business database was not restored"
! grep -q '^changed$' "$config_root/agent.env" || fail "environment was not restored"
[[ -f $backup_dir/BACKUP-METADATA ]] || fail "backup metadata is missing"

bad_env="$test_dir/bad.env"
cp "$config_root/agent.env" "$bad_env"
printf 'UNSAFE_KEY=value\n' >> "$bad_env"
expect_failure bash -c "source '$script_dir/lib/common.sh'; agent_validate_env_file '$bad_env' '$state_path'"

grep -q -- 'serve --env-file __ENV_FILE__' "$script_dir/systemd/wenzwork-device-agent.service" ||
  fail "systemd does not load credentials through the protected env file"
grep -q -- 'agent_create_backup' "$script_dir/upgrade.sh" || fail "upgrade does not create a complete backup"
grep -q -- 'agent_restore_backup' "$script_dir/upgrade.sh" || fail "upgrade does not restore data on failure"
grep -q -- 'configuration remains' "$script_dir/uninstall.sh" || fail "default uninstall does not document data preservation"
grep -q -- '/var/backups/wenzwork-device-agent' "$script_dir/uninstall.sh" || fail "explicit purge does not remove backups"
if grep -RE -- '--access-key([= ]|$)' "$script_dir/install.sh" "$script_dir/upgrade.sh" "$script_dir/lib"/*.sh >/dev/null; then
  fail "a script appears to pass the Device Access Key as a process argument"
fi

printf 'device_agent_scripts_test: PASS\n'
