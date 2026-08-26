#!/usr/bin/env bash
set -euo pipefail
[[ ${EUID:-$(id -u)} -eq 0 ]] || { printf 'run with sudo\n' >&2; exit 1; }
launchctl bootout system/com.wenzwork.relay >/dev/null 2>&1 || true
