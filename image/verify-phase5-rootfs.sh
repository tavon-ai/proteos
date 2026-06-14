#!/usr/bin/env bash
# verify-phase5-rootfs.sh — host-local Phase 5 acceptance for the baked rootfs.
#
# Run this on the KVM/Proxmox host (the box that ran build-rootfs.sh / the ansible
# node_agent role). It loop-mounts the baked image and checks the Phase 5 bits are
# really in it (task 5.5 done-when), WITHOUT needing the app stack or booting a VM:
#
#   - /etc/profile.d/proteos-providers.sh is installed,
#   - /etc/resolv.conf carries a static nameserver (the kernel ip= sets no DNS),
#   - a login shell actually sources an injected /run/proteos/env/*.env,
#   - (if Claude was baked) /usr/local/bin/claude exists + `claude --version` runs,
#     and /etc/claude-code/managed-settings.json + the manifest pin are present,
#   - /etc/proteos-release advertises the providers[,claude] feature set.
#
# The full browser→launch→write-a-file flow (task 5.7) needs the app stack on the
# app VM — see RUNBOOK Part E for that.
#
# Usage:
#   sudo image/verify-phase5-rootfs.sh [--image /var/lib/proteos/images/<baked>.ext4]
#                                      [--images-dir /var/lib/proteos/images]
#
# With no --image, it reads the `image = …` line from manifest.lock in --images-dir
# (default /var/lib/proteos/images), i.e. whatever the bake last produced.
#
# Linux + root (loop-mount + chroot). Non-destructive: it only creates and then
# removes a temp file under the image's /run (tmpfs at runtime anyway).
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
TEST_ENV_DIR="$MNT/run/proteos/env"
cleanup() {
  rm -f "$TEST_ENV_DIR/zz-verify.env" 2>/dev/null || true
  rmdir "$TEST_ENV_DIR" "$MNT/run/proteos" 2>/dev/null || true
  umount "$MNT" 2>/dev/null || true
  rmdir "$MNT" 2>/dev/null || true
}
trap cleanup EXIT

mount -o loop "$IMAGE" "$MNT"

# --- providers profile.d snippet (always baked) -----------------------------
SNIPPET="$MNT/etc/profile.d/proteos-providers.sh"
if [[ -f $SNIPPET ]]; then
  pass "profile.d snippet installed (/etc/profile.d/proteos-providers.sh)"
else
  fail "profile.d snippet MISSING (/etc/profile.d/proteos-providers.sh)"
fi

# --- static DNS resolver (kernel ip= cmdline sets no nameserver) -------------
# The guest boots with a static IP and no DHCP/systemd-resolved, so the rootfs
# must ship a real /etc/resolv.conf or every lookup fails ("Could not resolve
# host"). It must be a regular file (not the CI image's dangling symlink-to-stub)
# holding at least one nameserver.
RESOLV="$MNT/etc/resolv.conf"
if [[ -f $RESOLV && ! -L $RESOLV ]] && grep -Eq '^[[:space:]]*nameserver[[:space:]]+[0-9a-fA-F.:]+' "$RESOLV"; then
  ns="$(awk '/^[[:space:]]*nameserver/{print $2; exit}' "$RESOLV")"
  pass "static resolv.conf baked with a nameserver ($ns)"
elif [[ -L $RESOLV ]]; then
  fail "/etc/resolv.conf is a symlink (CI stub) — bake a static file instead"
else
  fail "/etc/resolv.conf missing or has no nameserver (guest will fail DNS)"
fi

# A login shell must source an injected env file. Simulate one runtime injection.
mkdir -p "$TEST_ENV_DIR"
printf "export ANTHROPIC_API_KEY='sk-proteos-verify-MARKER'\n" > "$TEST_ENV_DIR/zz-verify.env"
chmod 600 "$TEST_ENV_DIR/zz-verify.env"
if [[ -x "$MNT/bin/bash" || -x "$MNT/usr/bin/bash" ]]; then
  got="$(chroot "$MNT" /bin/bash -lc 'printf "%s" "${ANTHROPIC_API_KEY:-}"' 2>/dev/null || true)"
  if [[ $got == "sk-proteos-verify-MARKER" ]]; then
    pass "login shell sources injected /run/proteos/env/*.env"
  else
    fail "login shell did NOT source injected env (got '${got:-<empty>}')"
  fi
else
  skip "no bash in image — cannot test login-shell sourcing"
fi
rm -f "$TEST_ENV_DIR/zz-verify.env"
rmdir "$TEST_ENV_DIR" "$MNT/run/proteos" 2>/dev/null || true

# --- /etc/proteos-release feature set ---------------------------------------
RELEASE="$MNT/etc/proteos-release"
if [[ -f $RELEASE ]]; then
  feats="$(awk -F'=' '/^PROTEOS_GUESTAGENT_FEATURES=/{print $2}' "$RELEASE")"
  if [[ ",$feats," == *",providers,"* ]]; then
    pass "release advertises 'providers' feature ($feats)"
  else
    fail "release FEATURES missing 'providers' ($feats)"
  fi
else
  fail "/etc/proteos-release missing"
fi

# --- Claude Code (only if it was baked) -------------------------------------
CLAUDE="$MNT/usr/local/bin/claude"
if [[ -e $CLAUDE ]]; then
  if [[ -x $CLAUDE ]] && file "$CLAUDE" | grep -q ELF; then
    pass "claude installed (/usr/local/bin/claude, ELF, executable)"
  else
    fail "claude present but not an executable ELF"
  fi

  if [[ -f "$MNT/etc/claude-code/managed-settings.json" ]]; then
    pass "claude managed-settings baked (/etc/claude-code/managed-settings.json)"
  else
    fail "claude managed-settings MISSING"
  fi

  # Manifest pin present?
  if [[ -f $MANIFEST ]] && grep -Eq '^claude_version[[:space:]]*=[[:space:]]*[^( ]' "$MANIFEST"; then
    pass "manifest pins claude_version/sha256 ($(awk -F'=' '/^claude_version/{gsub(/ /,"",$2);print $2}' "$MANIFEST"))"
  else
    fail "manifest.lock has no real claude_version pin"
  fi

  # Best-effort: actually run it. --version is offline; failures here are not
  # fatal (the binary may need runtime bits a chroot lacks — confirm on a live VM).
  ver="$(chroot "$MNT" env HOME=/root DISABLE_AUTOUPDATER=1 /usr/local/bin/claude --version 2>/dev/null || true)"
  if [[ -n $ver ]]; then
    pass "claude --version runs in chroot: $ver"
  else
    skip "claude --version did not run in chroot (confirm on a booted VM — RUNBOOK Part E)"
  fi
else
  skip "no /usr/local/bin/claude — Claude Code was not baked into this image"
  skip "  (set proteos_claude_version / --claude-binary at bake time to include it)"
fi

echo
if [[ $FAILED -eq 0 ]]; then
  printf '\e[1;32m==> Phase 5 rootfs checks PASSED\e[0m\n'
  printf 'Next: the full browser→launch flow is RUNBOOK Part E (needs the app stack).\n'
else
  printf '\e[1;31m==> Phase 5 rootfs checks FAILED — see [FAIL] lines above\e[0m\n'
  exit 1
fi
