#!/usr/bin/env bash
# verify-phase8-rootfs.sh — host-local Phase 8 acceptance for the baked rootfs.
#
# Run this on the KVM/Proxmox host (the box that ran build-rootfs.sh / the ansible
# node_agent role). It loop-mounts the baked image and checks the Phase 8 editor
# bits are really in it, WITHOUT needing the app stack or booting a VM:
#
#   - code-server is installed and its entrypoint is runnable in the guest,
#   - the guest agent's systemd unit wires the web forward (PROTEOS_GUEST_WEB_LISTEN
#     on vsock:1025) and points PROTEOS_CODESERVER_BIN at the installed binary,
#   - NO editor auth/password/credential material is baked (the gateway is the
#     authenticator; code-server runs --auth none on a loopback bind),
#   - /etc/proteos-release advertises the `codeserver` feature.
#
# The live tunnel→code-server round-trip (boot a VM, dial port 1025, lazy-start)
# is covered by image/verify-phase8-live.sh (boots a real microVM).
#
# Usage:
#   sudo image/verify-phase8-rootfs.sh [--image /var/lib/proteos/images/<baked>.ext4]
#                                      [--images-dir /var/lib/proteos/images]
#
# With no --image, it reads the `image = …` line from manifest.lock in --images-dir
# (default /var/lib/proteos/images), i.e. whatever the bake last produced.
#
# Linux + root (loop-mount + chroot). Non-destructive.
set -euo pipefail

pass() { printf '\e[1;32m[ PASS ]\e[0m %s\n' "$*"; }
fail() { printf '\e[1;31m[ FAIL ]\e[0m %s\n' "$*"; FAILED=1; }
info() { printf '\e[1;34m[ .... ]\e[0m %s\n' "$*"; }
die() { printf '\e[1;31m[fatal]\e[0m %s\n' "$*" >&2; exit 2; }

IMAGE=""
IMAGES_DIR="/var/lib/proteos/images"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --image) IMAGE=$2; shift 2 ;;
    --images-dir) IMAGES_DIR=$2; shift 2 ;;
    *) die "unknown arg: $1" ;;
  esac
done

[[ "$(uname -s)" == "Linux" ]] || die "this script loop-mounts ext4 — run it on the Linux KVM host"
[[ $EUID -eq 0 ]] || die "run as root (sudo): loop-mount + chroot"

MANIFEST="$IMAGES_DIR/manifest.lock"
if [[ -z $IMAGE ]]; then
  [[ -f $MANIFEST ]] || die "no --image and no manifest at $MANIFEST"
  baked="$(awk -F'=' '/^image[[:space:]]*=/{gsub(/[[:space:]]/,"",$2); print $2}' "$MANIFEST")"
  [[ -n $baked && $baked != "(notyetbuilt)" ]] || die "manifest has no baked image name (run build-rootfs.sh first)"
  IMAGE="$IMAGES_DIR/$baked"
fi
[[ -f $IMAGE ]] || die "image not found: $IMAGE"
info "verifying $IMAGE"

FAILED=0
MNT="$(mktemp -d)"
BOUND=0
cleanup() {
  [[ $BOUND -eq 1 ]] && { umount "$MNT/dev" 2>/dev/null || true; umount "$MNT/proc" 2>/dev/null || true; }
  umount "$MNT" 2>/dev/null || true
  rmdir "$MNT" 2>/dev/null || true
}
trap cleanup EXIT

mount -o loop "$IMAGE" "$MNT"
# code-server's bundled node needs /dev,/proc for its dynamic loader in the chroot.
mount --bind /dev "$MNT/dev"
mount --bind /proc "$MNT/proc"
BOUND=1

CHROOT_PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# --- code-server present + runnable ------------------------------------------
CS_LINK="$MNT/usr/local/bin/code-server"
if [[ -e $CS_LINK ]]; then
  # Resolve the symlink target inside the image and confirm the entrypoint exists.
  if [[ -e "$MNT/usr/local/lib/code-server/bin/code-server" ]]; then
    pass "code-server installed (/usr/local/bin/code-server → lib/code-server/bin/code-server)"
  else
    fail "code-server symlink present but entrypoint missing under /usr/local/lib/code-server"
  fi
  # --version runs the bundled node; confirm it actually executes in the chroot.
  if CSVER="$(chroot "$MNT" /usr/bin/env "PATH=$CHROOT_PATH" code-server --version 2>/dev/null | head -1)"; then
    [[ -n $CSVER ]] && pass "code-server runs in the guest ($CSVER)" || fail "code-server --version produced no output"
  else
    fail "code-server is on PATH but '--version' did not run (bundled node broken?)"
  fi
else
  fail "code-server is NOT installed in the guest (the editor will not start)"
fi

# --- web-forward systemd wiring ----------------------------------------------
UNIT="$MNT/etc/systemd/system/proteos-guestagent.service"
if [[ -f $UNIT ]]; then
  if grep -qE '^Environment=PROTEOS_GUEST_WEB_LISTEN=vsock:1025' "$UNIT"; then
    pass "guest unit listens for the web forward on vsock:1025"
  else
    fail "guest unit missing PROTEOS_GUEST_WEB_LISTEN=vsock:1025 (the editor tunnel won't bind)"
  fi
  if grep -qE '^Environment=PROTEOS_CODESERVER_BIN=/usr/local/bin/code-server' "$UNIT"; then
    pass "guest unit points PROTEOS_CODESERVER_BIN at the installed binary"
  else
    fail "guest unit missing PROTEOS_CODESERVER_BIN (the forward can't supervise code-server)"
  fi
else
  fail "guest systemd unit not found at ${UNIT#"$MNT"}"
fi

# --- no editor auth/credential baked -----------------------------------------
# code-server is authenticated by the GATEWAY (it runs --auth none on a loopback
# bind); no password/hashed-password/cert may be baked into the image config.
LEAK=0
for cfg in "$MNT/root/.config/code-server/config.yaml" \
           "$MNT/home"/*/.config/code-server/config.yaml \
           "$MNT/etc/code-server/config.yaml"; do
  [[ -e $cfg ]] || continue
  info "found baked code-server config: ${cfg#"$MNT"}"
  if grep -qiE '^[[:space:]]*(password|hashed-password|cert)[[:space:]]*:' "$cfg" 2>/dev/null; then
    fail "baked code-server config carries auth material: ${cfg#"$MNT"}"
    LEAK=1
  fi
done
[[ $LEAK -eq 0 ]] && pass "no code-server auth/credential material baked into the image"

# --- release stamp advertises codeserver -------------------------------------
REL="$MNT/etc/proteos-release"
if [[ -f $REL ]] && grep -q 'PROTEOS_GUESTAGENT_FEATURES=.*codeserver' "$REL"; then
  pass "/etc/proteos-release advertises the codeserver feature"
  grep -E '^PROTEOS_(CODESERVER_VERSION|GUESTAGENT_FEATURES)=' "$REL" | sed 's/^/         /'
else
  fail "/etc/proteos-release does not advertise the codeserver feature"
fi

echo
if [[ $FAILED -eq 0 ]]; then
  pass "Phase 8 rootfs verification PASSED"
else
  fail "Phase 8 rootfs verification FAILED"
  exit 1
fi
