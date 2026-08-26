#!/usr/bin/env bash

set -Eeuo pipefail
umask 077

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
START_SCRIPT="$SCRIPT_DIR/start_server.sh"
UPGRADE_RUNNER=""

cleanup() {
  if [[ -n "$UPGRADE_RUNNER" && -f "$UPGRADE_RUNNER" ]]; then
    rm -f -- "$UPGRADE_RUNNER"
  fi
}

trap cleanup EXIT

usage() {
  cat <<'EOF'
Usage: ./upgrade_server.sh [PACKAGE [SHA256SUMS]]

Without arguments, reads GITHUB_RELEASE_REPOSITORY and GITHUB_ACCESS_TOKEN from
.env, downloads the latest Linux server package and checksum through the GitHub
Asset API, verifies SHA-256, backs up the current installation, stops the API,
installs the package, applies migrations, and restarts the service.

PACKAGE may also be a local path or an HTTP(S) URL. Supplying SHA256SUMS is
strongly recommended for a manually provided package.
EOF
}

case "${1:-}" in
  -h | --help | help)
    usage
    exit 0
    ;;
esac

(( $# <= 2 )) || {
  usage >&2
  exit 2
}

[[ -x "$START_SCRIPT" ]] || {
  printf '[wenzwork-upgrade] ERROR: start_server.sh is missing or not executable: %s\n' "$START_SCRIPT" >&2
  exit 1
}

# Run a private copy from the installation directory. The upgrade replaces
# start_server.sh in place; executing the original inode while it is truncated
# can make Bash resume from mismatched source bytes after a successful upgrade.
# Keeping the runner beside the installation preserves SCRIPT_DIR while
# isolating the active interpreter from package replacement.
UPGRADE_RUNNER="$(mktemp "$SCRIPT_DIR/.start_server.upgrade.XXXXXX")"
cp -p -- "$START_SCRIPT" "$UPGRADE_RUNNER"
chmod 0700 "$UPGRADE_RUNNER"
"$UPGRADE_RUNNER" upgrade "$@"
