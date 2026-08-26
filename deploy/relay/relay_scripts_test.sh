#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

fail() { printf 'relay_scripts_test: %s\n' "$*" >&2; exit 1; }
expect_failure() {
  if "$@" >/dev/null 2>&1; then
    fail "command unexpectedly succeeded: $*"
  fi
}

relay_validate_url https://downloads.example.test/relay/v1/package.tar.gz
relay_validate_url http://downloads.example.test/relay/v1/package.tar.gz
relay_validate_url http://localhost:8080/relay
relay_validate_url http://127.0.0.1:8080/relay
relay_validate_url 'http://[::1]:8080/relay'
expect_failure bash -c "source '$script_dir/lib/common.sh'; relay_validate_url ftp://downloads.example.test/package"
expect_failure bash -c "source '$script_dir/lib/common.sh'; relay_validate_url downloads.example.test/package"

[[ $(relay_validate_install_root /opt/wenzwork-relay/) == /opt/wenzwork-relay ]] ||
  fail "install root was not normalized"
relay_validate_install_root /srv/wenzwork/relay >/dev/null
expect_failure bash -c "source '$script_dir/lib/common.sh'; relay_validate_install_root /"
expect_failure bash -c "source '$script_dir/lib/common.sh'; relay_validate_install_root /opt"
expect_failure bash -c "source '$script_dir/lib/common.sh'; relay_validate_install_root '/srv/relay root'"
expect_failure bash -c "source '$script_dir/lib/common.sh'; relay_validate_install_root /srv/../etc/relay"

[[ $(relay_normalize_architecture x86_64) == amd64 ]] || fail "x86_64 did not map to amd64"
[[ $(relay_normalize_architecture amd64) == amd64 ]] || fail "amd64 did not remain amd64"
[[ $(relay_normalize_architecture aarch64) == arm64 ]] || fail "aarch64 did not map to arm64"
[[ $(relay_normalize_architecture arm64) == arm64 ]] || fail "arm64 did not remain arm64"
expect_failure bash -c "source '$script_dir/lib/common.sh'; relay_normalize_architecture i686"

for command in openssl sha256sum tar; do
  command -v "$command" >/dev/null 2>&1 || fail "$command is required for this test"
done
test_dir=$(mktemp -d)
cleanup() {
  [[ -n ${test_dir:-} && $test_dir == /tmp/* ]] && rm -rf -- "$test_dir"
}
trap cleanup EXIT

mkdir -p "$test_dir/fake-bin" "$test_dir/release/bin"
cat > "$test_dir/fake-bin/uname" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
  -s) printf 'Linux\n' ;;
  -m) printf '%s\n' "${RELAY_TEST_UNAME_ARCH:?}" ;;
  *) exit 2 ;;
esac
EOF
cat > "$test_dir/release/bin/relayctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" > "${RELAY_VERIFY_ARGS_FILE:?}"
EOF
chmod 0755 "$test_dir/fake-bin/uname" "$test_dir/release/bin/relayctl"
for architecture_case in 'x86_64:amd64' 'aarch64:arm64'; do
  host_arch=${architecture_case%%:*}
  manifest_arch=${architecture_case##*:}
  verify_args="$test_dir/verify-$manifest_arch.args"
  RELAY_TEST_UNAME_ARCH=$host_arch RELAY_VERIFY_ARGS_FILE=$verify_args \
    PATH="$test_dir/fake-bin:$PATH" relay_verify_release_tree "$test_dir/release" v1.2.3
  grep -q -- "--expected-platform linux" "$verify_args" || fail "manifest platform was not pinned to Linux"
  grep -q -- "--expected-architecture $manifest_arch" "$verify_args" ||
    fail "$host_arch did not select the $manifest_arch manifest target"
done

printf 'signed Relay test payload\n' > "$test_dir/payload.bin"
(
  cd "$test_dir"
  sha256sum payload.bin > SHA256SUMS
)
openssl genpkey -algorithm ED25519 -out "$test_dir/signing.key" >/dev/null 2>&1
openssl pkey -in "$test_dir/signing.key" -pubout -out "$test_dir/signing.pub" >/dev/null 2>&1
openssl pkeyutl -sign -rawin -inkey "$test_dir/signing.key" -in "$test_dir/SHA256SUMS" -out "$test_dir/SHA256SUMS.sig"
relay_verify_bundle "$test_dir/payload.bin" "$test_dir/SHA256SUMS" "$test_dir/SHA256SUMS.sig" "$test_dir/signing.pub"
relay_verify_same_public_key "$test_dir/signing.pub" "$test_dir/signing.pub"
openssl genpkey -algorithm ED25519 -out "$test_dir/other-signing.key" >/dev/null 2>&1
openssl pkey -in "$test_dir/other-signing.key" -pubout -out "$test_dir/other-signing.pub" >/dev/null 2>&1
expect_failure bash -c "source '$script_dir/lib/common.sh'; relay_verify_same_public_key '$test_dir/signing.pub' '$test_dir/other-signing.pub'"

printf 'tampered\n' >> "$test_dir/payload.bin"
expect_failure bash -c "source '$script_dir/lib/common.sh'; relay_verify_bundle '$test_dir/payload.bin' '$test_dir/SHA256SUMS' '$test_dir/SHA256SUMS.sig' '$test_dir/signing.pub'"

mkdir "$test_dir/archive-source"
printf 'safe\n' > "$test_dir/archive-source/file.txt"
tar -C "$test_dir/archive-source" -czf "$test_dir/safe.tar.gz" file.txt
relay_extract_bundle "$test_dir/safe.tar.gz" "$test_dir/extracted"
[[ $(<"$test_dir/extracted/file.txt") == safe ]] || fail "safe archive did not extract"
tar -C "$test_dir/archive-source" --transform='s|^|../|' -czf "$test_dir/unsafe.tar.gz" file.txt
expect_failure bash -c "source '$script_dir/lib/common.sh'; relay_extract_bundle '$test_dir/unsafe.tar.gz' '$test_dir/unsafe-output'"
if ln -s file.txt "$test_dir/archive-source/linked.txt" 2>/dev/null && [[ -L $test_dir/archive-source/linked.txt ]]; then
  tar -C "$test_dir/archive-source" -czf "$test_dir/symlink.tar.gz" linked.txt
  expect_failure bash -c "source '$script_dir/lib/common.sh'; relay_extract_bundle '$test_dir/symlink.tar.gz' '$test_dir/symlink-output'"
fi

token=$(printf 'a%.0s' {1..43})
[[ $(printf '%s\n' "$token" | relay_read_token true '') == "$token" ]] || fail "stdin token was not read"
token_file="$test_dir/token"
printf '%s\n' "$token" > "$token_file"
chmod 0600 "$token_file"
if [[ $(stat -c '%a' "$token_file") == 600 ]]; then
  [[ $(relay_read_token false "$token_file") == "$token" ]] || fail "0600 token file was not read"
  chmod 0644 "$token_file"
  expect_failure bash -c "source '$script_dir/lib/common.sh'; relay_read_token false '$token_file'"
else
  [[ $(uname -s) == MINGW* ]] || fail "test filesystem did not preserve mode 0600"
fi

access_key="relay_$(printf 'k%.0s' {1..43})"
[[ $(printf '%s\n' "$access_key" | relay_read_access_key true '') == "$access_key" ]] ||
  fail "stdin Access Key was not read"
access_key_file="$test_dir/access-key"
printf '%s\n' "$access_key" > "$access_key_file"
chmod 0600 "$access_key_file"
if [[ $(stat -c '%a' "$access_key_file") == 600 ]]; then
  [[ $(relay_read_access_key false "$access_key_file") == "$access_key" ]] ||
    fail "0600 Access Key file was not read"
  chmod 0644 "$access_key_file"
  expect_failure bash -c "source '$script_dir/lib/common.sh'; relay_read_access_key false '$access_key_file'"
fi

relay_env_file="$test_dir/relay.env"
printf 'RELAY_ACCESS_KEY=%s\nRELAY_MANAGEMENT_URL=https://control.example.test\n' "$access_key" > "$relay_env_file"
chmod 0600 "$relay_env_file"
if [[ $(stat -c '%a' "$relay_env_file") == 600 ]]; then
  relay_validate_env_file "$relay_env_file"
  printf 'RELAY_ACCESS_KEY=invalid\n' > "$relay_env_file"
  expect_failure bash -c "source '$script_dir/lib/common.sh'; relay_validate_env_file '$relay_env_file'"
fi

grep -q -- '--artifact-url' "$script_dir/upgrade.sh" || fail "upgrade script does not support URL artifacts"
grep -q -- 'relay_installed_root' "$script_dir/upgrade.sh" || fail "upgrade script does not reuse the installed work directory"
grep -q -- 'RELAY_VERSION' "$script_dir/lib/common.sh" || fail "Relay version is not persisted in Access Key environments"
grep -q -- 'relay_preflight_host' "$script_dir/upgrade.sh" || fail "upgrade script does not validate the host platform"

if grep -RE -- '--(token|enrollment-token)[= ]+\$?[^-]' "$script_dir"/*.sh "$script_dir/lib"/*.sh >/dev/null; then
  fail "a script appears to pass a token as a command argument"
fi
if grep -RE -- '--access-key[= ]+' "$script_dir"/*.sh "$script_dir/lib"/*.sh >/dev/null; then
  fail "a script appears to pass an Access Key as a command argument"
fi
printf 'relay_scripts_test: PASS\n'
