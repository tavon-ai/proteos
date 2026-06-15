#!/usr/bin/env bash
# verify-phase8-live.sh — live Phase 8 acceptance on the firecracker/KVM host.
#
# Boots a REAL Firecracker microVM from the baked rootfs and proves the editor
# tunnel works end-to-end at the driver level: dial the guest web vsock port
# (1025), the guest agent's web forward lazily starts code-server, and an HTTP
# request returns a code-server response. This is the host-side half of the Phase
# 8 live acceptance (the app-stack half — open the editor from the dashboard,
# edit+save, logout-kills-editor, DevTools cookie audit — is in the RUNBOOK).
#
# It drives the firecracker integration test TestGuestWebForwardCodeServer, the
# same harness the ansible acceptance gate uses, so it needs root + /dev/kvm + the
# pinned firecracker/jailer/cryptsetup binaries and a rootfs with code-server baked.
#
# Usage:
#   sudo image/verify-phase8-live.sh [--images-dir /var/lib/proteos/images]
#                                    [--kernel <name>] [--rootfs <name>]
#
# With no --rootfs it reads the baked image from manifest.lock in --images-dir.
# Run on a FRESH node (or stop the node-agent first): it boots a VM and allocates
# from the guest subnet, so it contends with live machines otherwise.
set -euo pipefail

info() { printf '\e[1;34m[ .... ]\e[0m %s\n' "$*"; }
die()  { printf '\e[1;31m[fatal]\e[0m %s\n' "$*" >&2; exit 2; }

IMAGES_DIR="/var/lib/proteos/images"
KERNEL="vmlinux"
ROOTFS=""
FIRECRACKER_BIN="${PROTEOS_FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${PROTEOS_JAILER_BIN:-/usr/local/bin/jailer}"
CRYPTSETUP_BIN="${PROTEOS_CRYPTSETUP_BIN:-/usr/sbin/cryptsetup}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --images-dir) IMAGES_DIR=$2; shift 2 ;;
    --kernel) KERNEL=$2; shift 2 ;;
    --rootfs) ROOTFS=$2; shift 2 ;;
    *) die "unknown arg: $1" ;;
  esac
done

[[ "$(uname -s)" == "Linux" ]] || die "boots a microVM — run on the Linux firecracker host"
[[ $EUID -eq 0 ]] || die "run as root (sudo): jailer + netlink + /dev/kvm"
[[ -e /dev/kvm ]] || die "/dev/kvm not present (KVM required)"
command -v go >/dev/null || die "go toolchain required to run the integration test"

MANIFEST="$IMAGES_DIR/manifest.lock"
if [[ -z $ROOTFS ]]; then
  [[ -f $MANIFEST ]] || die "no --rootfs and no manifest at $MANIFEST"
  ROOTFS="$(awk -F'=' '/^image[[:space:]]*=/{gsub(/[[:space:]]/,"",$2); print $2}' "$MANIFEST")"
  [[ -n $ROOTFS ]] || die "manifest has no baked image name (run build-rootfs.sh first)"
fi
[[ -f "$IMAGES_DIR/$ROOTFS" ]] || die "rootfs not found: $IMAGES_DIR/$ROOTFS"
[[ -f "$IMAGES_DIR/$KERNEL" ]] || die "kernel not found: $IMAGES_DIR/$KERNEL"

# Sanity: the rootfs must actually carry code-server (else this can never pass).
if ! grep -q 'codeserver' "$MANIFEST" 2>/dev/null; then
  info "WARNING: manifest.lock does not list a codeserver feature — was the image baked with --no-codeserver?"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NODEAGENT_DIR="$(cd "$SCRIPT_DIR/../nodeagent" && pwd)"

info "booting a microVM from $ROOTFS and testing the editor tunnel (port 1025)…"
cd "$NODEAGENT_DIR"
GOWORK=off \
PROTEOS_TEST_KERNEL="$IMAGES_DIR/$KERNEL" \
PROTEOS_TEST_ROOTFS="$IMAGES_DIR/$ROOTFS" \
PROTEOS_TEST_ROOTFS_HAS_GUEST_AGENT=1 \
PROTEOS_FIRECRACKER_BIN="$FIRECRACKER_BIN" \
PROTEOS_JAILER_BIN="$JAILER_BIN" \
PROTEOS_CRYPTSETUP_BIN="$CRYPTSETUP_BIN" \
  go test -tags firecracker -count=1 -timeout 10m -v \
    -run TestGuestWebForwardCodeServer ./internal/driver/firecracker/

echo
printf '\e[1;32m[ PASS ]\e[0m Phase 8 live editor tunnel verified.\n'
