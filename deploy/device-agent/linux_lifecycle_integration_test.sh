#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'linux_lifecycle_integration_test: %s\n' "$*" >&2
  exit 1
}

[[ ${WENZWORK_AGENT_LIFECYCLE_EPHEMERAL_ROOT:-} == I_UNDERSTAND_THIS_MUTATES_AN_EPHEMERAL_ROOT ]] ||
  fail 'explicit ephemeral-root confirmation is required'
[[ ${EUID:-$(id -u)} -eq 0 ]] || fail 'the lifecycle integration test must run as root'
if [[ ! -f /.dockerenv ]]; then
  [[ ${GITHUB_ACTIONS:-} == true && ${RUNNER_ENVIRONMENT:-} == github-hosted ]] ||
    fail 'run only in a disposable container or a GitHub-hosted runner'
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
repo_root=$(cd -- "$script_dir/../.." && pwd -P)
for command in bash openssl sha256sum tar python3 find getent groupadd useradd userdel groupdel; do
  command -v "$command" >/dev/null 2>&1 || fail "required command is missing: $command"
done

managed_paths=(
  /opt/wenzwork-device-agent
  /etc/wenzwork-device-agent
  /var/lib/wenzwork-device-agent
  /var/backups/wenzwork-device-agent
  /etc/systemd/system/wenzwork-device-agent.service
)
for managed_path in "${managed_paths[@]}"; do
  [[ ! -e $managed_path && ! -L $managed_path ]] ||
    fail "refusing to overwrite a pre-existing managed path: $managed_path"
done
getent passwd wenzwork-agent >/dev/null 2>&1 && fail 'wenzwork-agent user already exists'
getent group wenzwork-agent >/dev/null 2>&1 && fail 'wenzwork-agent group already exists'

test_dir=$(mktemp -d /tmp/wenzwork-device-agent-lifecycle.XXXXXX)
# shellcheck disable=SC2329 # Invoked indirectly by trap.
cleanup() {
  [[ ${test_dir:-} == /tmp/wenzwork-device-agent-lifecycle.?????? ]] || return 1
  rm -rf -- "$test_dir"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$test_dir/fake-bin" /run/systemd/system
cat > "$test_dir/fake-bin/systemctl" <<'SYSTEMCTL'
#!/usr/bin/env bash
set -euo pipefail

state_root=/run/wenzwork-device-agent-lifecycle
active_file="$state_root/active"
enabled_file="$state_root/enabled"
mkdir -p "$state_root"

start_service() {
  local version=''
  if [[ -f /opt/wenzwork-device-agent/current/VERSION ]]; then
    version=$(tr -d '[:space:]' < /opt/wenzwork-device-agent/current/VERSION)
  fi
  if [[ $version == v3-bad ]]; then
    printf 'mutated-by-v3\n' > /var/lib/wenzwork-device-agent/state/business.txt
    rm -f -- "$active_file"
    return 1
  fi
  : > "$active_file"
}

action=${1:-}
shift || true
case "$action" in
  daemon-reload)
    ;;
  enable)
    : > "$enabled_file"
    for argument in "$@"; do
      [[ $argument != --now ]] || start_service
    done
    ;;
  disable)
    rm -f -- "$enabled_file" "$active_file"
    ;;
  start)
    start_service
    ;;
  stop)
    rm -f -- "$active_file"
    ;;
  is-active)
    [[ -f $active_file ]]
    ;;
  show)
    if [[ -f $active_file ]]; then printf 'running\n'; else printf 'dead\n'; fi
    ;;
  *)
    printf 'unsupported fake systemctl action: %s\n' "$action" >&2
    exit 64
    ;;
esac
SYSTEMCTL
chmod 0755 "$test_dir/fake-bin/systemctl"
export PATH="$test_dir/fake-bin:$PATH"

openssl genpkey -algorithm ED25519 -out "$test_dir/signing.key" >/dev/null 2>&1
openssl pkey -in "$test_dir/signing.key" -pubout -out "$test_dir/signing.pub" >/dev/null 2>&1

architecture=$(uname -m)
case "$architecture" in
  x86_64|amd64) architecture=amd64 ;;
  aarch64|arm64) architecture=arm64 ;;
  *) fail "unsupported integration-test architecture: $architecture" ;;
esac

make_package() {
  local version=$1 reported_version=${2:-$1} root archive checksums signature
  root="$test_dir/package-$version"
  archive="$test_dir/wenzwork-device-agent-$version-linux-$architecture.tar.gz"
  checksums="$test_dir/SHA256SUMS-$version"
  signature="$test_dir/SHA256SUMS-$version.sig"
  mkdir -p "$root/bin" "$root/scripts/lib" "$root/systemd"

  install -m 0755 "$repo_root/deploy/device-agent/install.sh" "$root/scripts/install.sh"
  install -m 0755 "$repo_root/deploy/device-agent/upgrade.sh" "$root/scripts/upgrade.sh"
  install -m 0755 "$repo_root/deploy/device-agent/healthcheck.sh" "$root/scripts/healthcheck.sh"
  install -m 0755 "$repo_root/deploy/device-agent/uninstall.sh" "$root/scripts/uninstall.sh"
  install -m 0644 "$repo_root/deploy/device-agent/lib/common.sh" "$root/scripts/lib/common.sh"
  install -m 0644 "$repo_root/deploy/device-agent/systemd/wenzwork-device-agent.service" \
    "$root/systemd/wenzwork-device-agent.service"
  install -m 0600 "$repo_root/deploy/device-agent/device-agent.env.example" "$root/device-agent.env.example"
  install -m 0644 "$test_dir/signing.pub" "$root/release-signing-public-key.pem"
  printf '%s\n' "$version" > "$root/VERSION"

  cat > "$root/bin/wenzwork-device-agent" <<AGENT
#!/usr/bin/env bash
set -euo pipefail
if [[ \${1:-} == version || \${1:-} == --version ]]; then
  printf '%s\\n' '$reported_version'
  exit 0
fi
exit 64
AGENT
  chmod 0755 "$root/bin/wenzwork-device-agent"

  cat > "$root/bin/relayctl" <<'RELAYCTL'
#!/usr/bin/env bash
set -euo pipefail
[[ ${1:-} == release && ${2:-} == verify ]] || exit 64
shift 2
root=''
manifest=''
expected_version=''
expected_platform=''
expected_architecture=''
protocol_version=''
while (($#)); do
  case "$1" in
    --root) root=${2:-}; shift 2 ;;
    --manifest) manifest=${2:-}; shift 2 ;;
    --expected-version) expected_version=${2:-}; shift 2 ;;
    --expected-platform) expected_platform=${2:-}; shift 2 ;;
    --expected-architecture) expected_architecture=${2:-}; shift 2 ;;
    --protocol-version) protocol_version=${2:-}; shift 2 ;;
    *) exit 64 ;;
  esac
done
python3 - "$root" "$manifest" "$expected_version" "$expected_platform" "$expected_architecture" "$protocol_version" <<'PY'
import hashlib
import json
import os
import pathlib
import sys

root, manifest_name, version, platform, architecture, protocol = sys.argv[1:]
root_path = pathlib.Path(root).resolve(strict=True)
if pathlib.PurePosixPath(manifest_name).is_absolute() or '..' in pathlib.PurePosixPath(manifest_name).parts:
    raise SystemExit(1)
manifest_path = root_path / manifest_name
document = json.loads(manifest_path.read_text(encoding='utf-8'))
if document.get('schemaVersion') != 1 or document.get('version') != version:
    raise SystemExit(1)
if document.get('platform') != platform or document.get('architecture') != architecture:
    raise SystemExit(1)
if not (int(document.get('protocolMin', 0)) <= int(protocol) <= int(document.get('protocolMax', 0))):
    raise SystemExit(1)

listed = set()
for record in document.get('files', []):
    relative = record.get('path', '')
    pure = pathlib.PurePosixPath(relative)
    if not relative or pure.is_absolute() or '..' in pure.parts or '\\' in relative or relative in listed:
        raise SystemExit(1)
    listed.add(relative)
    candidate = (root_path / pathlib.Path(*pure.parts)).resolve(strict=True)
    if os.path.commonpath((str(root_path), str(candidate))) != str(root_path):
        raise SystemExit(1)
    if not candidate.is_file() or candidate.is_symlink():
        raise SystemExit(1)
    payload = candidate.read_bytes()
    if len(payload) != int(record.get('size', -1)):
        raise SystemExit(1)
    if hashlib.sha256(payload).hexdigest() != record.get('sha256'):
        raise SystemExit(1)

actual = set()
for directory, directories, files in os.walk(root_path, followlinks=False):
    for name in directories:
        if (pathlib.Path(directory) / name).is_symlink():
            raise SystemExit(1)
    for name in files:
        candidate = pathlib.Path(directory) / name
        if candidate.is_symlink():
            raise SystemExit(1)
        relative = candidate.relative_to(root_path).as_posix()
        if relative != manifest_name:
            actual.add(relative)
if actual != listed:
    raise SystemExit(1)
PY
RELAYCTL
  chmod 0755 "$root/bin/relayctl"

  python3 - "$root" "$version" "$architecture" <<'PY'
import hashlib
import json
import os
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
records = []
for directory, _, files in os.walk(root):
    for name in files:
        candidate = pathlib.Path(directory) / name
        relative = candidate.relative_to(root).as_posix()
        if relative == 'release-manifest.json':
            continue
        payload = candidate.read_bytes()
        records.append({'path': relative, 'sha256': hashlib.sha256(payload).hexdigest(), 'size': len(payload)})
records.sort(key=lambda item: item['path'])
document = {
    'schemaVersion': 1,
    'version': sys.argv[2],
    'platform': 'linux',
    'architecture': sys.argv[3],
    'protocolMin': 1,
    'protocolMax': 1,
    'commit': 'a' * 40,
    'buildTimeUnix': 1,
    'signingKeyId': 'integration-test',
    'files': records,
}
(root / 'release-manifest.json').write_text(json.dumps(document, separators=(',', ':')) + '\n', encoding='utf-8')
PY

  tar -C "$root" -czf "$archive" .
  (cd "$test_dir" && sha256sum "$(basename "$archive")" > "$(basename "$checksums")")
  openssl pkeyutl -sign -rawin -inkey "$test_dir/signing.key" -in "$checksums" -out "$signature"
}

package_arguments() {
  local version=$1
  printf '%s\n' \
    --package-file "$test_dir/wenzwork-device-agent-$version-linux-$architecture.tar.gz" \
    --checksums-file "$test_dir/SHA256SUMS-$version" \
    --checksums-signature-file "$test_dir/SHA256SUMS-$version.sig" \
    --signing-key-file "$test_dir/signing.pub"
}

make_package v1
make_package v2
make_package v3-bad
mapfile -t v1_arguments < <(package_arguments v1)
mapfile -t v2_arguments < <(package_arguments v2)
mapfile -t v3_arguments < <(package_arguments v3-bad)

access_key="device_$(printf 'k%.0s' {1..43})"
cat > "$test_dir/agent.env" <<ENV
WENZWORK_CONTROL_URL=https://control.example.test
WENZWORK_DEVICE_ACCESS_KEY=$access_key
WENZWORK_DEVICE_STATE_FILE=/var/lib/wenzwork-device-agent/state/agent-state.json
WENZWORK_DEVICE_WORKSPACE=/var/lib/wenzwork-device-agent/workspace
WENZWORK_AGENT_SECRET_STORE=file
ENV
chmod 0600 "$test_dir/agent.env"

groupadd --system wenzwork-agent
useradd --system --gid wenzwork-agent --home-dir /tmp/not-the-agent-data --shell /bin/bash wenzwork-agent
if bash "$repo_root/deploy/device-agent/install.sh" "${v1_arguments[@]}" --agent-env-file "$test_dir/agent.env"; then
  fail 'installer accepted a pre-existing mismatched service identity'
fi
userdel wenzwork-agent
groupdel wenzwork-agent 2>/dev/null || true

bash "$repo_root/deploy/device-agent/install.sh" "${v1_arguments[@]}" --agent-env-file "$test_dir/agent.env"
[[ $(tr -d '[:space:]' < /opt/wenzwork-device-agent/current/VERSION) == v1 ]] || fail 'v1 was not installed'
[[ -f /run/wenzwork-device-agent-lifecycle/active ]] || fail 'service was not started after installation'
printf 'business-v1\n' > /var/lib/wenzwork-device-agent/state/business.txt
printf 'encrypted-secret-v1\n' > /var/lib/wenzwork-device-agent/state/agent-state.json.secrets.enc
chown -R wenzwork-agent:wenzwork-agent /var/lib/wenzwork-device-agent

bash "$repo_root/deploy/device-agent/upgrade.sh" "${v2_arguments[@]}" --confirm-upgrade
[[ $(tr -d '[:space:]' < /opt/wenzwork-device-agent/current/VERSION) == v2 ]] || fail 'v2 was not activated'
[[ $(< /var/lib/wenzwork-device-agent/state/business.txt) == business-v1 ]] || fail 'successful upgrade changed business data'
backup_count=$(find /var/backups/wenzwork-device-agent -mindepth 1 -maxdepth 1 -type d ! -name '.backup.*' | wc -l)
[[ $backup_count -eq 1 ]] || fail 'successful upgrade did not retain exactly one pre-upgrade backup'

printf 'before-failed-v3\n' > /var/lib/wenzwork-device-agent/state/business.txt
printf 'before-failed-secret\n' > /var/lib/wenzwork-device-agent/state/agent-state.json.secrets.enc
if bash "$repo_root/deploy/device-agent/upgrade.sh" "${v3_arguments[@]}" --confirm-upgrade; then
  fail 'intentionally failed v3 upgrade unexpectedly succeeded'
fi
[[ $(tr -d '[:space:]' < /opt/wenzwork-device-agent/current/VERSION) == v2 ]] || fail 'failed upgrade did not restore v2'
[[ $(< /var/lib/wenzwork-device-agent/state/business.txt) == before-failed-v3 ]] || fail 'failed upgrade did not restore business data'
[[ $(< /var/lib/wenzwork-device-agent/state/agent-state.json.secrets.enc) == before-failed-secret ]] ||
  fail 'failed upgrade did not restore encrypted secrets'
failed_root=$(find /var/lib -mindepth 1 -maxdepth 1 -type d -name 'wenzwork-device-agent.failed.*' -print -quit)
[[ -n $failed_root && $(< "$failed_root/state/business.txt") == mutated-by-v3 ]] ||
  fail 'failed-upgrade data was not preserved for diagnostics'

bash "$repo_root/deploy/device-agent/uninstall.sh" --confirm
[[ ! -e /opt/wenzwork-device-agent ]] || fail 'default uninstall retained binaries'
[[ ! -e /etc/systemd/system/wenzwork-device-agent.service ]] || fail 'default uninstall retained the service unit'
[[ -f /var/lib/wenzwork-device-agent/state/business.txt ]] || fail 'default uninstall deleted business data'
[[ -f /etc/wenzwork-device-agent/agent.env ]] || fail 'default uninstall deleted configuration'
[[ -d /var/backups/wenzwork-device-agent ]] || fail 'default uninstall deleted backups'

bash "$repo_root/deploy/device-agent/install.sh" "${v2_arguments[@]}"
[[ $(< /var/lib/wenzwork-device-agent/state/business.txt) == before-failed-v3 ]] || fail 'reinstall did not reuse preserved business data'
bash "$repo_root/deploy/device-agent/uninstall.sh" --confirm --purge --confirm-purge DELETE_DEVICE_AGENT_DATA
for managed_path in "${managed_paths[@]}"; do
  [[ ! -e $managed_path && ! -L $managed_path ]] || fail "explicit purge retained $managed_path"
done
getent passwd wenzwork-agent >/dev/null 2>&1 && fail 'explicit purge retained the service user'
getent group wenzwork-agent >/dev/null 2>&1 && fail 'explicit purge retained the service group'

printf 'linux_lifecycle_integration_test: PASS\n'
