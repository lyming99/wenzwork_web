#!/usr/bin/env bash
set -euo pipefail

log() { printf '[wenzwork-relay-control] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

package_dir=''
environment_file=''
while (($#)); do
  case "$1" in
    --package-dir) package_dir=${2:-}; shift 2 ;;
    --environment-file) environment_file=${2:-}; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ ${EUID:-$(id -u)} -eq 0 ]] || die "run with sudo or as root"
[[ -d /run/systemd/system ]] || die "systemd is required"
[[ -n $package_dir && -d $package_dir ]] || die "--package-dir is required"
[[ -n $environment_file && -f $environment_file ]] || die "--environment-file is required"
package_dir=$(cd -- "$package_dir" && pwd -P)
environment_file=$(cd -- "$(dirname -- "$environment_file")" && printf '%s/%s\n' "$PWD" "$(basename -- "$environment_file")")

for binary in wenzwork-relay-directory wenzwork-relay-scheduler; do
  [[ -x $package_dir/bin/$binary ]] || die "package is missing bin/$binary"
done
for unit in wenzwork-relay-directory.service wenzwork-relay-scheduler.service; do
  [[ -f $package_dir/systemd/$unit ]] || die "package is missing systemd/$unit"
done
grep -Eq '^DATABASE_URL=.+$' "$environment_file" || die "environment file must define DATABASE_URL"

if ! getent group wenzwork-control >/dev/null; then groupadd --system wenzwork-control; fi
if ! id wenzwork-control >/dev/null 2>&1; then
  useradd --system --gid wenzwork-control --home-dir /var/lib/wenzwork-control --shell /usr/sbin/nologin wenzwork-control
fi
install -d -o root -g root -m 0755 /opt/wenzwork-control/bin
install -d -o root -g wenzwork-control -m 0750 /etc/wenzwork /var/lib/wenzwork-control
install -o root -g wenzwork-control -m 0640 "$environment_file" /etc/wenzwork/relay-control.env
for binary in wenzwork-relay-directory wenzwork-relay-scheduler; do
  install -o root -g root -m 0755 "$package_dir/bin/$binary" "/opt/wenzwork-control/bin/$binary"
done
for unit in wenzwork-relay-directory.service wenzwork-relay-scheduler.service; do
  install -o root -g root -m 0644 "$package_dir/systemd/$unit" "/etc/systemd/system/$unit"
done

systemctl daemon-reload
systemctl enable --now wenzwork-relay-directory.service wenzwork-relay-scheduler.service
systemctl is-active --quiet wenzwork-relay-directory.service
systemctl is-active --quiet wenzwork-relay-scheduler.service
log "Relay control services installed. Verify mTLS Directory reachability and local readiness endpoints before activation."
