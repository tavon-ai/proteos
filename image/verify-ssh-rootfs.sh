#!/usr/bin/env bash
# verify-ssh-rootfs.sh — host-local acceptance for the inbound-SSH bits of the
# baked rootfs.
#
# Run this on the KVM/Proxmox host (the box that ran build-rootfs.sh / the ansible
# node_agent role). It loop-mounts the baked image and checks the SSH layer is
# really in it, WITHOUT needing the app stack or booting a VM:
#
#   - sshd + ssh-keygen are installed and runnable in the guest, and its config
#     parses (`sshd -t`),
#   - the config is loopback-only, key-only and forwarding-free — the properties
#     that make the guest agent's vsock forward the SOLE path to sshd,
#   - proteos-sshd.service is installed AND enabled (multi-user.target.wants),
#     and the stock wildcard-bound ssh.service/ssh.socket are not,
#   - the guest agent's unit wires the SSH forward (PROTEOS_GUEST_SSH_LISTEN on
#     vsock:1027) — without this the host's vsock CONNECT to 1027 gets EOF,
#   - NO host keys are baked (each machine generates its own on first boot),
#   - /etc/proteos-release advertises the `ssh` feature.
#
# The live round-trip (boot a VM, dial the guest SSH port, read the banner) is
# covered by TestGuestSSHForward in the firecracker KVM suite.
#
# Usage:
#   sudo image/verify-ssh-rootfs.sh [--image /var/lib/proteos/images/<baked>.ext4]
#                                   [--images-dir /var/lib/proteos/images]
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
# sshd's dynamic loader (and `sshd -t`) want /dev,/proc in the chroot.
mount --bind /dev "$MNT/dev"
mount --bind /proc "$MNT/proc"
BOUND=1

CHROOT_PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# --- sshd present + its config parses ----------------------------------------
if [[ -x "$MNT/usr/sbin/sshd" ]]; then
  SSHVER="$(chroot "$MNT" /usr/bin/env "PATH=$CHROOT_PATH" sh -c 'ssh -V' 2>&1 | head -1)"
  pass "sshd installed (${SSHVER:-version unknown})"
  # `sshd -t` is the real parser: it also fails on a missing privsep user, so it
  # covers the extract-only install's skipped postinst.
  if OUT="$(chroot "$MNT" /usr/bin/env "PATH=$CHROOT_PATH" sshd -t -f /etc/ssh/sshd_config 2>&1)"; then
    pass "sshd config parses in the guest (sshd -t)"
  else
    # A clean image has no host keys yet (they are generated on first boot), so
    # -t's "no hostkeys available" is expected and NOT a config error.
    if grep -qi 'no hostkeys available' <<<"$OUT" && ! grep -qiE 'line [0-9]+|bad configuration|unsupported option' <<<"$OUT"; then
      pass "sshd config parses in the guest (only the expected 'no hostkeys available' — generated on first boot)"
    else
      fail "sshd -t rejected the baked config: $OUT"
    fi
  fi
else
  fail "sshd is NOT installed in the guest (the SSH forward has nothing to bridge to)"
fi
if [[ -x "$MNT/usr/bin/ssh-keygen" ]]; then
  pass "ssh-keygen present (the unit's ExecStartPre generates host keys on first boot)"
else
  fail "ssh-keygen missing — 'ssh-keygen -A' in proteos-sshd.service will fail and sshd will never start"
fi
if chroot "$MNT" /usr/bin/env "PATH=$CHROOT_PATH" id -u sshd >/dev/null 2>&1; then
  pass "sshd privilege-separation user exists"
else
  fail "no 'sshd' user — sshd refuses to start without its privsep account"
fi

# --- the config is loopback-only, key-only, forwarding-free -------------------
CFG="$MNT/etc/ssh/sshd_config"
if [[ -f $CFG ]]; then
  check_cfg() { # check_cfg REGEX DESC FAILMSG
    if grep -qE "$1" "$CFG"; then pass "$2"; else fail "$3"; fi
  }
  check_cfg '^ListenAddress[[:space:]]+127\.0\.0\.1$' \
    "sshd binds loopback only (the vsock forward is the sole path in)" \
    "sshd_config does not pin ListenAddress 127.0.0.1 — sshd may bind the guest's tap NIC"
  check_cfg '^PasswordAuthentication[[:space:]]+no$' \
    "password auth disabled (injected keys only)" \
    "sshd_config does not disable PasswordAuthentication"
  check_cfg '^KbdInteractiveAuthentication[[:space:]]+no$' \
    "keyboard-interactive auth disabled" \
    "sshd_config does not disable KbdInteractiveAuthentication"
  check_cfg '^PubkeyAuthentication[[:space:]]+yes$' \
    "public-key auth enabled" \
    "sshd_config does not enable PubkeyAuthentication (no one could log in)"
  check_cfg '^PermitRootLogin[[:space:]]+(no|prohibit-password)$' \
    "root password login refused" \
    "sshd_config permits root password login"
  check_cfg '^AllowTcpForwarding[[:space:]]+no$' \
    "TCP forwarding disabled (no unaudited egress path around the node's nft rules)" \
    "sshd_config allows TCP forwarding — a second, unaudited egress path"
  if grep -qE '^Include[[:space:]]' "$CFG"; then
    fail "sshd_config includes drop-ins — a stray /etc/ssh/sshd_config.d file could widen this policy"
  else
    pass "sshd_config is self-contained (no drop-in Include)"
  fi
else
  fail "no /etc/ssh/sshd_config in the image"
fi

# --- the unit is installed AND enabled; the stock ones are not ----------------
SSH_UNIT="$MNT/etc/systemd/system/proteos-sshd.service"
if [[ -f $SSH_UNIT ]]; then
  pass "proteos-sshd.service installed"
  if [[ -L "$MNT/etc/systemd/system/multi-user.target.wants/proteos-sshd.service" ]]; then
    pass "proteos-sshd.service enabled at boot (multi-user.target.wants)"
  else
    fail "proteos-sshd.service is NOT enabled — sshd will not start at boot"
  fi
  grep -qE '^ExecStartPre=.*ssh-keygen -A' "$SSH_UNIT" \
    && pass "unit generates missing host keys before sshd starts" \
    || fail "unit has no 'ssh-keygen -A' ExecStartPre — a fresh machine has no host keys and sshd will not start"
  grep -qE '^RuntimeDirectory=sshd$' "$SSH_UNIT" \
    && pass "unit creates the /run/sshd privsep directory" \
    || fail "unit lacks RuntimeDirectory=sshd — sshd exits with 'Missing privilege separation directory'"
else
  fail "proteos-sshd.service not found at ${SSH_UNIT#"$MNT"}"
fi
STRAY=0
for stray in "$MNT/etc/systemd/system/multi-user.target.wants/ssh.service" \
  "$MNT/etc/systemd/system/sockets.target.wants/ssh.socket"; do
  if [[ -e $stray ]]; then
    fail "stock ${stray##*/} is ENABLED — it binds every interface, bypassing the loopback-only policy"
    STRAY=1
  fi
done
if [[ $STRAY -eq 0 ]]; then
  pass "the stock wildcard-bound ssh.service/ssh.socket are not enabled"
fi

# --- guest-agent SSH forward wiring ------------------------------------------
UNIT="$MNT/etc/systemd/system/proteos-guestagent.service"
if [[ -f $UNIT ]]; then
  if grep -qE '^Environment=PROTEOS_GUEST_SSH_LISTEN=vsock:1027' "$UNIT"; then
    pass "guest unit listens for the SSH forward on vsock:1027"
  else
    fail "guest unit missing PROTEOS_GUEST_SSH_LISTEN=vsock:1027 (the forward never binds; the host's vsock CONNECT gets EOF)"
  fi
else
  fail "guest systemd unit not found at ${UNIT#"$MNT"}"
fi

# --- no baked host keys ------------------------------------------------------
# A shared host identity across the fleet would let any machine impersonate any
# other; keys are generated per machine on first boot and persisted by the agent.
shopt -s nullglob
baked_keys=("$MNT"/etc/ssh/ssh_host_*)
shopt -u nullglob
if [[ ${#baked_keys[@]} -eq 0 ]]; then
  pass "no host keys baked (each machine generates its own on first boot)"
else
  fail "image bakes ${#baked_keys[@]} host key file(s) — every machine would share one SSH identity"
fi

# --- release stamp advertises ssh ---------------------------------------------
REL="$MNT/etc/proteos-release"
if [[ -f $REL ]] && grep -q 'PROTEOS_GUESTAGENT_FEATURES=.*ssh' "$REL"; then
  pass "/etc/proteos-release advertises the ssh feature"
  grep -E '^PROTEOS_(OPENSSH_VERSION|GUESTAGENT_FEATURES)=' "$REL" | sed 's/^/         /'
else
  fail "/etc/proteos-release does not advertise the ssh feature"
fi

echo
if [[ $FAILED -eq 0 ]]; then
  pass "SSH rootfs verification PASSED"
else
  fail "SSH rootfs verification FAILED"
  exit 1
fi
