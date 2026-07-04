#!/usr/bin/env bash
# verify-phase7-rootfs.sh — host-local Phase 7 acceptance for the baked rootfs.
#
# Run this on the KVM/Proxmox host (the box that ran build-rootfs.sh / the ansible
# node_agent role). It loop-mounts the baked image and checks the Phase 7 git bits
# are really in it, WITHOUT needing the app stack or booting a VM:
#
#   - git is installed and runnable in the guest (clone/commit/push need it),
#   - the guestagent binary is present and its `git-credential` subcommand runs
#     (it is the credential helper git invokes — Phase 7 decision #5),
#   - NO gitconfig / git credential / token material is baked into the image
#     (identity is pushed at runtime; tokens are fetched on demand, never on disk),
#   - /etc/proteos-release advertises the `git` feature.
#
# The full clone/commit/push flow (task 7.6) needs the app stack with the real
# GitHub App — see the Phase 7 plan / RUNBOOK. The control-channel + credential
# relay are covered by `go test ./internal/guestctl/...` in normal CI.
#
# Usage:
#   sudo image/verify-phase7-rootfs.sh [--image /var/lib/proteos/images/<baked>.ext4]
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
skip() { printf '\e[1;33m[ SKIP ]\e[0m %s\n' "$*"; }
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
# git needs /dev,/proc for its dynamic loader / runtime in the chroot.
mount --bind /dev "$MNT/dev"
mount --bind /proc "$MNT/proc"
BOUND=1

CHROOT_PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# --- git present + runnable ---------------------------------------------------
if chroot "$MNT" /usr/bin/env "PATH=$CHROOT_PATH" sh -c 'command -v git' >/dev/null 2>&1; then
  GITVER="$(chroot "$MNT" /usr/bin/env "PATH=$CHROOT_PATH" git --version 2>/dev/null || true)"
  if [[ -n $GITVER ]]; then
    pass "git installed and runnable in guest ($GITVER)"
  else
    fail "git is on PATH but 'git --version' did not run"
  fi
else
  fail "git is NOT installed in the guest (clone/commit/push will fail)"
fi

# --- guestagent binary + git-credential subcommand ---------------------------
GA="$MNT/usr/local/bin/guestagent"
if [[ -x $GA ]] && file "$GA" | grep -q ELF; then
  pass "guestagent installed (/usr/local/bin/guestagent, ELF, executable)"
  # The `store` action is a no-op that must exit 0 with empty stdin: it proves the
  # subcommand dispatch exists without needing the live agent socket.
  if printf '' | chroot "$MNT" /usr/bin/env "PATH=$CHROOT_PATH" /usr/local/bin/guestagent git-credential store >/dev/null 2>&1; then
    pass "guestagent git-credential subcommand runs (store no-op exits 0)"
  else
    fail "guestagent git-credential subcommand missing or errored"
  fi
else
  fail "guestagent binary missing or not an executable ELF"
fi

# --- gh wrapper mints GH_TOKEN via the credential helper ----------------------
# gh does not use git's credential.helper, so /usr/local/bin/gh must be the
# wrapper that fetches a fresh token from `guestagent git-credential` and execs
# the real binary at /usr/local/libexec/proteos/gh. gh is optional (--no-gh).
GH_BIN="$MNT/usr/local/bin/gh"
GH_REAL="$MNT/usr/local/libexec/proteos/gh"
if [[ -e $GH_BIN || -e $GH_REAL ]]; then
  if [[ -x $GH_BIN ]] && head -1 "$GH_BIN" | grep -q '^#!' \
    && grep -q 'guestagent git-credential' "$GH_BIN"; then
    pass "gh wrapper in place (/usr/local/bin/gh mints GH_TOKEN via guestagent git-credential)"
  else
    fail "/usr/local/bin/gh is not the auth wrapper (gh will run unauthenticated)"
  fi
  if [[ -x $GH_REAL ]] && file "$GH_REAL" | grep -q ELF; then
    pass "real gh binary installed (/usr/local/libexec/proteos/gh, ELF, executable)"
  else
    fail "real gh binary missing at /usr/local/libexec/proteos/gh (wrapper has nothing to exec)"
  fi
else
  skip "gh not baked (--no-gh)"
fi

# --- no secret / gitconfig baked ---------------------------------------------
# Identity is pushed at runtime (git.configure); tokens are fetched on demand and
# never written to disk. The image must therefore carry no gitconfig and no
# credential store anywhere under home dirs.
LEAK=0
for f in "$MNT/root/.gitconfig" "$MNT/root/.git-credentials" "$MNT/etc/gitconfig"; do
  if [[ -e $f ]]; then
    info "found baked git file: ${f#"$MNT"}"
    if grep -qiE 'token|password|x-access-token|gh[posru]_[A-Za-z0-9]' "$f" 2>/dev/null; then
      fail "baked git file contains credential-like material: ${f#"$MNT"}"
      LEAK=1
    fi
  fi
done
# A baked /etc/gitconfig with only [safe] or system defaults is acceptable; a
# baked ~/.gitconfig is not expected (it is pushed at runtime).
if [[ -e "$MNT/root/.gitconfig" ]]; then
  fail "/root/.gitconfig is baked into the image (it must be pushed at runtime)"
elif [[ $LEAK -eq 0 ]]; then
  pass "no gitconfig / credential material baked into the image"
fi

# --- release stamp advertises git --------------------------------------------
REL="$MNT/etc/proteos-release"
if [[ -f $REL ]] && grep -q 'PROTEOS_GUESTAGENT_FEATURES=.*git' "$REL"; then
  pass "/etc/proteos-release advertises the git feature"
  grep -E '^PROTEOS_(GIT_VERSION|GUESTAGENT_FEATURES)=' "$REL" | sed 's/^/         /'
else
  fail "/etc/proteos-release does not advertise the git feature"
fi

echo
if [[ $FAILED -eq 0 ]]; then
  pass "Phase 7 rootfs verification PASSED"
else
  fail "Phase 7 rootfs verification FAILED"
  exit 1
fi
