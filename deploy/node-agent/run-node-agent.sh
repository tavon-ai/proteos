#!/usr/bin/env bash
# run-node-agent.sh — launch the ProteOS node-agent with the Firecracker driver
# on a bare KVM host (the Proxmox VM).
#
# The node-agent CANNOT be containerised: it creates tap devices, writes
# nftables rules, and execs the jailer, so it runs natively as root on the host
# that has /dev/kvm. The control-plane stack (db + control-plane + web) runs
# elsewhere (see deploy/app-stack/) and dials this agent over the network.
#
# Usage:
#   sudo ./run-node-agent.sh          # preflight, build if needed, run (foreground)
#   sudo ./run-node-agent.sh build    # build the binary only, then exit
#
# Config comes from the environment. For convenience this script sources an env
# file if present: $PROTEOS_AGENT_ENV_FILE, else ./.env next to this script.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# --- load env file ----------------------------------------------------------
ENV_FILE="${PROTEOS_AGENT_ENV_FILE:-$SCRIPT_DIR/.env}"
if [[ -f "$ENV_FILE" ]]; then
  set -a; . "$ENV_FILE"; set +a
fi

# --- defaults (override via env / .env) -------------------------------------
: "${PROTEOS_AGENT_ADDR:=0.0.0.0:9090}"          # 0.0.0.0 so the app VM can reach it
: "${PROTEOS_AGENT_DRIVER:=firecracker}"
: "${PROTEOS_AGENT_DATA_DIR:=/var/lib/proteos/agent}"
: "${PROTEOS_AGENT_IMAGES_DIR:=/var/lib/proteos/images}"
: "${PROTEOS_CHROOT_BASE_DIR:=/srv/jailer}"
: "${PROTEOS_FIRECRACKER_BIN:=/usr/local/bin/firecracker}"
: "${PROTEOS_JAILER_BIN:=/usr/local/bin/jailer}"
: "${PROTEOS_AGENT_SUBNET:=172.30.0.0/24}"
# Phase 4: per-machine LUKS volumes (disk + snapshot, encrypted at rest) and the
# cryptsetup binary. The volumes dir MUST live outside PROTEOS_CHROOT_BASE_DIR.
: "${PROTEOS_AGENT_VOLUMES_DIR:=/var/lib/proteos/volumes}"
: "${PROTEOS_CRYPTSETUP_BIN:=/usr/sbin/cryptsetup}"
# Preflight-only: must match the control plane's PROTEOS_KERNEL_REF/ROOTFS_REF.
: "${PROTEOS_KERNEL_REF:=vmlinux}"
: "${PROTEOS_ROOTFS_REF:=ubuntu-24.04.ext4}"
: "${BIN_OUT:=/usr/local/bin/proteos-node-agent}"

export PROTEOS_AGENT_ADDR PROTEOS_AGENT_DRIVER PROTEOS_AGENT_DATA_DIR \
       PROTEOS_AGENT_IMAGES_DIR PROTEOS_CHROOT_BASE_DIR PROTEOS_FIRECRACKER_BIN \
       PROTEOS_JAILER_BIN PROTEOS_AGENT_SUBNET PROTEOS_AGENT_TOKEN \
       PROTEOS_AGENT_VOLUMES_DIR PROTEOS_CRYPTSETUP_BIN
# Optional TLS for the agent channel (Phase 4 decision #3): export only if both
# are set, so an unset pair stays plain HTTP.
[[ "${PROTEOS_AGENT_TLS_CERT:-}" && "${PROTEOS_AGENT_TLS_KEY:-}" ]] && \
  export PROTEOS_AGENT_TLS_CERT PROTEOS_AGENT_TLS_KEY

fail() { echo "[fail] $*" >&2; exit 1; }
ok()   { echo "[ ok ] $*"; }

build() {
  command -v go >/dev/null || fail "go toolchain not found (needed to build the node-agent)"
  echo "building node-agent (-tags=firecracker) -> $BIN_OUT"
  # nodeagent is a self-contained module (no external deps); build it in
  # isolation so the workspace/control-plane tree is irrelevant here.
  ( cd "$REPO_ROOT/nodeagent" && GOWORK=off go build -tags=firecracker -o "$BIN_OUT" ./cmd/nodeagent )
  ok "built $BIN_OUT"
}

preflight() {
  [[ "${PROTEOS_AGENT_TOKEN:-}" ]] || fail "PROTEOS_AGENT_TOKEN is required (must match the control plane)"
  [[ $EUID -eq 0 ]] || fail "must run as root (jailer + tap + nftables) — re-run with sudo"
  [[ -r /dev/kvm ]] || fail "/dev/kvm not present/readable — is nested virtualization enabled?"
  [[ -x "$PROTEOS_FIRECRACKER_BIN" ]] || fail "firecracker not executable at $PROTEOS_FIRECRACKER_BIN (set PROTEOS_FIRECRACKER_BIN)"
  [[ -x "$PROTEOS_JAILER_BIN" ]]      || fail "jailer not executable at $PROTEOS_JAILER_BIN (set PROTEOS_JAILER_BIN)"
  command -v ip  >/dev/null || fail "'ip' (iproute2) not found"
  command -v nft >/dev/null || fail "'nft' (nftables) not found"
  # Phase 4: cryptsetup is needed to format/open the per-machine LUKS volumes.
  [[ -x "$PROTEOS_CRYPTSETUP_BIN" ]] || command -v cryptsetup >/dev/null || \
    fail "cryptsetup not found at $PROTEOS_CRYPTSETUP_BIN (set PROTEOS_CRYPTSETUP_BIN; apt install cryptsetup)"
  [[ -r "$PROTEOS_AGENT_IMAGES_DIR/$PROTEOS_KERNEL_REF" ]] || fail "kernel image missing: $PROTEOS_AGENT_IMAGES_DIR/$PROTEOS_KERNEL_REF"
  [[ -r "$PROTEOS_AGENT_IMAGES_DIR/$PROTEOS_ROOTFS_REF" ]] || fail "rootfs image missing: $PROTEOS_AGENT_IMAGES_DIR/$PROTEOS_ROOTFS_REF"
  # The volumes dir holds encrypted disk+snapshot containers; keep it 0700 and
  # OUTSIDE the jail base so jail teardown never deletes machine state.
  mkdir -p "$PROTEOS_AGENT_DATA_DIR" "$PROTEOS_CHROOT_BASE_DIR" "$PROTEOS_AGENT_VOLUMES_DIR"
  chmod 700 "$PROTEOS_AGENT_VOLUMES_DIR"
  ok "preflight passed (kvm, firecracker, jailer, ip, nft, cryptsetup, images)"
}

case "${1:-run}" in
  build)
    build
    ;;
  run)
    preflight
    [[ -x "$BIN_OUT" ]] || build
    echo "starting node-agent on $PROTEOS_AGENT_ADDR (driver=$PROTEOS_AGENT_DRIVER)"
    exec "$BIN_OUT"
    ;;
  *)
    fail "unknown subcommand: ${1} (use 'run' or 'build')"
    ;;
esac
