#!/usr/bin/env bash
set -euo pipefail

mode=ready
wait_seconds=0
while (($#)); do
  case "$1" in
    --live) mode=live; shift ;;
    --ready) mode=ready; shift ;;
    --wait) wait_seconds=${2:-0}; shift 2 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
[[ $wait_seconds =~ ^[0-9]+$ ]] || exit 2
deadline=$((SECONDS + wait_seconds))
until curl --fail --silent --show-error --max-time 3 "http://127.0.0.1:19090/health/$mode"; do
  (( SECONDS < deadline )) || exit 1
  sleep 1
done
