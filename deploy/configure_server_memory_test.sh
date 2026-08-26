#!/usr/bin/env bash

set -Eeuo pipefail

REPOSITORY_ROOT="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/wenzwork-memory-test.XXXXXX")"
INSTALL_DIR="$TEST_ROOT/install"
FAKE_BIN_DIR="$TEST_ROOT/fake-bin"
MEMINFO_FILE="$TEST_ROOT/meminfo"
FSTAB_FILE="$TEST_ROOT/fstab"
SWAP_FILE="$TEST_ROOT/swapfile"
SWAP_STATE="$TEST_ROOT/swap.state"
CALL_LOG="$TEST_ROOT/calls.log"

cleanup() {
  rm -rf -- "$TEST_ROOT"
}

trap cleanup EXIT

mkdir -p "$INSTALL_DIR" "$FAKE_BIN_DIR"
cp "$REPOSITORY_ROOT/deploy/configure_server_memory.sh" "$INSTALL_DIR/configure_server_memory.sh"
chmod 0755 "$INSTALL_DIR/configure_server_memory.sh"

cat > "$INSTALL_DIR/.env" <<'EOF'
APP_ENV=production
DATABASE_URL=postgres://memory-test
export GOMEMLIMIT=999MiB
WENZWORK_MEMORY_HIGH=999M
WENZWORK_MEMORY_HIGH=888M
EOF
chmod 600 "$INSTALL_DIR/.env"

cat > "$INSTALL_DIR/start_server.sh" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${1:-}" == "install-systemd" ]]
grep -Fxq 'GOMEMLIMIT=256MiB' "$(dirname -- "$0")/.env"
grep -Fxq 'WENZWORK_MEMORY_HIGH=384M' "$(dirname -- "$0")/.env"
grep -Fxq 'WENZWORK_MEMORY_MAX=512M' "$(dirname -- "$0")/.env"
printf 'install-systemd\n' >> "$WENZWORK_TEST_CALL_LOG"
EOF
chmod 0755 "$INSTALL_DIR/start_server.sh"

cat > "$MEMINFO_FILE" <<'EOF'
MemTotal:        2000000 kB
MemAvailable:    1500000 kB
SwapTotal:             0 kB
EOF
printf '# test fstab\n' > "$FSTAB_FILE"

cat > "$FAKE_BIN_DIR/fallocate" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${1:-}" == "-l" && -n "${2:-}" && -n "${3:-}" ]]
printf 'fallocate:%s:%s\n' "$2" "$3" >> "$WENZWORK_TEST_CALL_LOG"
: > "$3"
EOF

cat > "$FAKE_BIN_DIR/mkswap" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ -f "${1:-}" ]]
printf 'mkswap:%s\n' "$1" >> "$WENZWORK_TEST_CALL_LOG"
EOF

cat > "$FAKE_BIN_DIR/swapon" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
case "${1:-}" in
  --noheadings)
    [[ "${2:-}" == "--show=NAME" ]]
    [[ ! -f "$WENZWORK_TEST_SWAP_STATE" ]] || cat "$WENZWORK_TEST_SWAP_STATE"
    ;;
  *)
    [[ -f "${1:-}" ]]
    printf '%s\n' "$1" > "$WENZWORK_TEST_SWAP_STATE"
    printf 'swapon:%s\n' "$1" >> "$WENZWORK_TEST_CALL_LOG"
    ;;
esac
EOF
chmod 0755 "$FAKE_BIN_DIR/fallocate" "$FAKE_BIN_DIR/mkswap" "$FAKE_BIN_DIR/swapon"

export PATH="$FAKE_BIN_DIR:$PATH"
export WENZWORK_ALLOW_NON_LINUX_MEMORY_CONFIG=1
export WENZWORK_ALLOW_NON_ROOT_MEMORY_CONFIG=1
export WENZWORK_MEMINFO_FILE="$MEMINFO_FILE"
export WENZWORK_FSTAB_FILE="$FSTAB_FILE"
export WENZWORK_SWAP_FILE="$SWAP_FILE"
export WENZWORK_TEST_SWAP_STATE="$SWAP_STATE"
export WENZWORK_TEST_CALL_LOG="$CALL_LOG"

"$INSTALL_DIR/configure_server_memory.sh" >/dev/null

[[ "$(grep -Fc 'GOMEMLIMIT=256MiB' "$INSTALL_DIR/.env")" -eq 1 ]]
[[ "$(grep -Fc 'WENZWORK_MEMORY_HIGH=384M' "$INSTALL_DIR/.env")" -eq 1 ]]
[[ "$(grep -Fc 'WENZWORK_MEMORY_MAX=512M' "$INSTALL_DIR/.env")" -eq 1 ]]
[[ "$(grep -Fc "$SWAP_FILE none swap sw 0 0" "$FSTAB_FILE")" -eq 1 ]]
[[ "$(cat "$SWAP_STATE")" == "$SWAP_FILE" ]]
[[ "$(grep -Fc 'install-systemd' "$CALL_LOG")" -eq 1 ]]
[[ "$(find "$INSTALL_DIR" -maxdepth 1 -name '.env.wenzwork-backup.*' | wc -l)" -eq 1 ]]
[[ "$(find "$TEST_ROOT" -maxdepth 1 -name 'fstab.wenzwork-backup.*' | wc -l)" -eq 1 ]]

"$INSTALL_DIR/configure_server_memory.sh" >/dev/null

[[ "$(grep -Fc 'fallocate:' "$CALL_LOG")" -eq 1 ]]
[[ "$(grep -Fc 'mkswap:' "$CALL_LOG")" -eq 1 ]]
[[ "$(grep -Fc 'swapon:' "$CALL_LOG")" -eq 1 ]]
[[ "$(grep -Fc 'install-systemd' "$CALL_LOG")" -eq 2 ]]
[[ "$(grep -Fc "$SWAP_FILE none swap sw 0 0" "$FSTAB_FILE")" -eq 1 ]]
[[ "$(find "$INSTALL_DIR" -maxdepth 1 -name '.env.wenzwork-backup.*' | wc -l)" -eq 1 ]]
[[ "$(find "$TEST_ROOT" -maxdepth 1 -name 'fstab.wenzwork-backup.*' | wc -l)" -eq 1 ]]

printf '2 GiB memory profile and idempotent swap configuration test passed\n'
