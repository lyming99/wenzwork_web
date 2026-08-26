#!/usr/bin/env bash

set -Eeuo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
UPGRADE_SCRIPT="$SCRIPT_DIR/upgrade_server.sh"

printf '[wenzwork-upgrade] NOTE: upgrage_server.sh is kept as a compatibility alias; prefer upgrade_server.sh.\n' >&2
exec "$UPGRADE_SCRIPT" "$@"
