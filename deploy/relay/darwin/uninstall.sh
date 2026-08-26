#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

purge=false
confirmed=false
while (($#)); do
  case "$1" in
    --purge) purge=true; shift ;;
    --confirm) confirmed=true; shift ;;
    *) relay_die "unknown argument: $1" ;;
  esac
done
relay_require_root
if [[ $confirmed == false ]]; then
  [[ -t 0 ]] || relay_die "pass --confirm for non-interactive uninstall"
  answer=''
  IFS= read -r -p '输入 UNINSTALL 移除 WenzWork Relay: ' answer
  [[ $answer == UNINSTALL ]] || relay_die "uninstall cancelled"
fi
install_root=$(relay_installed_root)
launchctl bootout system/com.wenzwork.relay >/dev/null 2>&1 || true
rm -f -- /Library/LaunchDaemons/com.wenzwork.relay.plist
if [[ -L /usr/local/bin/relayctl ]]; then rm -f -- /usr/local/bin/relayctl; fi
if [[ $purge == true ]]; then
  [[ $install_root == /usr/local/* && $install_root != /usr/local ]] || relay_die "refusing unsafe purge path"
  rm -rf -- "$install_root" '/Library/Application Support/WenzWork/Relay'
else
  relay_log "binaries and configuration preserved; pass --purge to delete them"
fi
relay_log "macOS Relay service removed"
