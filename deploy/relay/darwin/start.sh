#!/usr/bin/env bash
set -euo pipefail
[[ ${EUID:-$(id -u)} -eq 0 ]] || { printf 'run with sudo\n' >&2; exit 1; }
plist=/Library/LaunchDaemons/com.wenzwork.relay.plist
[[ -f $plist ]] || { printf 'Relay LaunchDaemon is not installed\n' >&2; exit 1; }
launchctl print system/com.wenzwork.relay >/dev/null 2>&1 || launchctl bootstrap system "$plist"
launchctl enable system/com.wenzwork.relay
launchctl kickstart -k system/com.wenzwork.relay
