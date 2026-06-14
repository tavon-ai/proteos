#!/usr/bin/env bash
# build-rootfs.sh — bake the ProteOS guest rootfs (Phase 3 decision #2).
#
# Takes the pinned Firecracker-CI base ext4, installs the statically-linked
# guest agent binary (Phase 4: with the persist + SQLite + /resume support — it
# mounts the node-agent-attached /dev/vdb at /persist and bind-mounts $HOME/
# workspace onto it), the proteos-guestagent systemd unit (enabled at boot), and
# an /etc/proteos-release stamp, then emits:
#
#     proteos-rootfs-<base>-ga<gitshort>.ext4   (the baked image)
#     manifest.lock                              (sha256 + provenance — commit it)
#
# This is the pinned MANUAL seed of Phase 12's image pipeline: one offline build
# step, no per-boot injection machinery (snapshot-friendly — Phase 4 restores RAM
# and an init-time injector would be dead code there).
#
# Linux-only: it loop-mounts the ext4 (needs sudo + a Linux kernel). Run it on
# the Proxmox host (or any Linux box); it is NOT part of `go test`.
#
# Usage:
#   image/build-rootfs.sh --base /path/to/firecracker-ci-ubuntu-24.04.ext4 \
#                         [--out-dir image] [--grow-mib 256] \
#                         [ --claude-bootstrap [--claude-version stable|latest|X.Y.Z]
#                         | --claude-binary /path/to/claude --claude-version X.Y.Z
#                           [--claude-sha256 <hex>] ]
#
# Phase 5: bake the Claude Code agent CLI into /usr/local/bin/claude (pinned by
# version + sha256, recorded in manifest.lock). Two ways to supply it:
#
#   --claude-bootstrap : fetch the official native binary from Anthropic's release
#       endpoint (downloads.claude.ai) and verify it against the published
#       manifest.json checksum — the same artifact + integrity check the upstream
#       bootstrap.sh uses, minus its per-$HOME `claude install` launcher step
#       (wrong here: Phase 4 bind-mounts the persistent home over /root at runtime,
#       so a $HOME launcher would be shadowed — we install to the system path).
#       --claude-version pins the channel/version (default: stable). Needs network
#       on the build host.
#   --claude-binary    : bake a pre-fetched pinned binary (fully offline / air-gap).
#       Requires --claude-version; --claude-sha256 verifies it.
#
# Omitting both bakes the providers profile.d wiring but no agent CLI (it warns).
#
# The base must be a systemd image (the unit is installed for systemd); the
# script asserts this and fails loudly otherwise.

set -euo pipefail

log() { printf '\e[1;34m[rootfs]\e[0m %s\n' "$*"; }
ok() { printf '\e[1;32m[ ok ]\e[0m %s\n' "$*"; }
die() {
  printf '\e[1;31m[fail]\e[0m %s\n' "$*" >&2
  exit 1
}

# dl URL [OUTFILE] — fetch over curl or wget (whichever is present).
dl() {
  if command -v curl >/dev/null 2>&1; then
    if [[ -n ${2:-} ]]; then curl -fsSL -o "$2" "$1"; else curl -fsSL "$1"; fi
  elif command -v wget >/dev/null 2>&1; then
    if [[ -n ${2:-} ]]; then wget -qO "$2" "$1"; else wget -qO - "$1"; fi
  else
    die "need curl or wget for --claude-bootstrap"
  fi
}

# fetch_claude_official TARGET DEST — download the pinned Claude Code native
# binary from Anthropic's release endpoint and verify it against the published
# manifest.json checksum (the same artifact + integrity check bootstrap.sh uses).
# TARGET is "stable", "latest", or an exact X.Y.Z. Sets CLAUDE_VERSION/CLAUDE_SHA256.
fetch_claude_official() {
  local target="$1" dest="$2"
  local base="https://downloads.claude.ai/claude-code-releases"

  local arch platform
  case "$(uname -m)" in
    x86_64 | amd64) arch="x64" ;;
    arm64 | aarch64) arch="arm64" ;;
    *) die "unsupported arch for Claude Code: $(uname -m)" ;;
  esac
  platform="${CLAUDE_PLATFORM:-linux-$arch}"  # base is glibc; pass --claude-platform for musl

  # Resolve a channel to a concrete version; accept an explicit X.Y.Z as-is.
  local version
  if [[ "$target" =~ ^[0-9]+\.[0-9]+\.[0-9]+ ]]; then
    version="$target"
  else
    version="$(dl "$base/$target")" || die "resolve Claude channel '$target'"
  fi
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+ ]] || die "bad Claude version resolved from '$target': $version"

  local manifest checksum
  manifest="$(dl "$base/$version/manifest.json")" || die "fetch Claude manifest for $version"
  if command -v jq >/dev/null 2>&1; then
    checksum="$(printf '%s' "$manifest" | jq -r ".platforms[\"$platform\"].checksum // empty")"
  else
    # bash fallback: collapse whitespace, then regex the platform's checksum.
    checksum="$(printf '%s' "$manifest" | tr -d '\n\r\t' \
      | grep -oE "\"$platform\"[^}]*\"checksum\"[[:space:]]*:[[:space:]]*\"[a-f0-9]{64}\"" \
      | grep -oE '[a-f0-9]{64}' | head -1)"
  fi
  [[ "$checksum" =~ ^[a-f0-9]{64}$ ]] || die "no checksum for platform '$platform' in Claude manifest $version"

  dl "$base/$version/$platform/claude" "$dest" || die "download Claude binary $version/$platform"
  local actual
  actual="$(sha256sum "$dest" | awk '{print $1}')"
  [[ "$actual" == "$checksum" ]] || die "Claude checksum mismatch: manifest $checksum, got $actual"
  chmod +x "$dest"
  CLAUDE_VERSION="$version"
  CLAUDE_SHA256="$checksum"
  ok "fetched Claude Code $version ($platform), sha256 verified against manifest"
}

# --- args -------------------------------------------------------------------
BASE=""
OUT_DIR=""
GROW_MIB=256
CLAUDE_BIN=""
CLAUDE_VERSION=""
CLAUDE_SHA256=""
CLAUDE_BOOTSTRAP=0
CLAUDE_PLATFORM=""   # override the auto-detected downloads.claude.ai platform (e.g. linux-x64-musl)
while [[ $# -gt 0 ]]; do
  case "$1" in
    --base) BASE=$2; shift 2 ;;
    --out-dir) OUT_DIR=$2; shift 2 ;;
    --grow-mib) GROW_MIB=$2; shift 2 ;;
    --claude-binary) CLAUDE_BIN=$2; shift 2 ;;
    --claude-version) CLAUDE_VERSION=$2; shift 2 ;;
    --claude-sha256) CLAUDE_SHA256=$2; shift 2 ;;
    --claude-bootstrap) CLAUDE_BOOTSTRAP=1; shift ;;
    --claude-platform) CLAUDE_PLATFORM=$2; shift 2 ;;
    *) die "unknown arg: $1" ;;
  esac
done
[[ -n $BASE ]] || die "--base <pinned firecracker-ci ext4> is required"
[[ -f $BASE ]] || die "base rootfs not found: $BASE"
if [[ -n $CLAUDE_BIN ]]; then
  [[ $CLAUDE_BOOTSTRAP -eq 0 ]] || die "use either --claude-binary or --claude-bootstrap, not both"
  [[ -f $CLAUDE_BIN ]] || die "claude binary not found: $CLAUDE_BIN"
  [[ -n $CLAUDE_VERSION ]] || die "--claude-version is required with --claude-binary (pins the manifest)"
fi

# Resolve repo paths relative to this script.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
OUT_DIR="${OUT_DIR:-$SCRIPT_DIR}"
UNIT_SRC="$SCRIPT_DIR/proteos-guestagent.service"
[[ -f $UNIT_SRC ]] || die "missing unit file: $UNIT_SRC"
PROFILE_SRC="$SCRIPT_DIR/profile.d-proteos-providers.sh"
[[ -f $PROFILE_SRC ]] || die "missing profile.d snippet: $PROFILE_SRC"
CLAUDE_SETTINGS_SRC="$SCRIPT_DIR/claude-managed-settings.json"
[[ -f $CLAUDE_SETTINGS_SRC ]] || die "missing claude managed settings: $CLAUDE_SETTINGS_SRC"

command -v sudo >/dev/null || die "sudo required (loop-mount)"
command -v go >/dev/null || die "go toolchain required to build the guest agent"
[[ "$(uname -s)" == "Linux" ]] || die "this script loop-mounts ext4 — run it on Linux"

# --- 1. build the static guest agent ----------------------------------------
# Provenance SHA for the image name. Honour an explicit override (set by the
# Ansible bake step, whose rsynced source tree has no .git to resolve) and fall
# back to the checkout's own HEAD.
GIT_SHORT="${PROTEOS_ROOTFS_GIT_SHORT:-$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo nogit)}"
VERSION="ga-$GIT_SHORT"
BASE_NAME="$(basename "$BASE" .ext4)"
OUT_IMG="$OUT_DIR/proteos-rootfs-${BASE_NAME}-ga${GIT_SHORT}.ext4"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT  # replaced by the full cleanup once the image is mounted
GA_BIN="$WORK/guestagent"
log "building static guest agent ($VERSION)"
( cd "$REPO_ROOT/guestagent" &&
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$GA_BIN" ./cmd/guestagent )
file "$GA_BIN" | grep -q "statically linked\|ELF" || die "guest agent is not a static ELF"
ok "built $GA_BIN"

# Phase 5: in --claude-bootstrap mode, fetch + verify the official native binary
# now (before the loop-mount), so a network/checksum failure aborts cleanly.
if [[ $CLAUDE_BOOTSTRAP -eq 1 ]]; then
  CLAUDE_TARGET="${CLAUDE_VERSION:-stable}"
  log "fetching Claude Code via Anthropic release endpoint (target: $CLAUDE_TARGET)"
  CLAUDE_BIN="$WORK/claude"
  fetch_claude_official "$CLAUDE_TARGET" "$CLAUDE_BIN"
fi

# --- 2. copy + grow the base image -------------------------------------------
# The Claude Code native binary is large (~240 MiB) and the default headroom is
# sized for the guest agent alone, so when baking Claude grow enough to fit it
# plus margin (unless the caller already asked for more).
if [[ -n $CLAUDE_BIN ]]; then
  CLAUDE_MIB="$(du -m "$CLAUDE_BIN" | awk '{print $1}')"
  NEED_MIB=$((CLAUDE_MIB + 128))
  if [[ $GROW_MIB -lt $NEED_MIB ]]; then
    log "claude binary is ${CLAUDE_MIB}MiB — bumping grow ${GROW_MIB}→${NEED_MIB}MiB"
    GROW_MIB=$NEED_MIB
  fi
fi
log "copying base image → $OUT_IMG (+${GROW_MIB}MiB headroom)"
cp "$BASE" "$OUT_IMG"
# Grow so the binary + unit always fit, then fsck/resize.
dd if=/dev/zero bs=1M count="$GROW_MIB" >>"$OUT_IMG" 2>/dev/null
e2fsck -fy "$OUT_IMG" >/dev/null 2>&1 || true
resize2fs "$OUT_IMG" >/dev/null 2>&1 || die "resize2fs failed"

# --- 3. mount + install ------------------------------------------------------
MNT="$WORK/mnt"
mkdir -p "$MNT"
cleanup() {
  sudo umount "$MNT" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

log "loop-mounting"
sudo mount -o loop "$OUT_IMG" "$MNT"

# Assert the base is a systemd image — the unit we install is systemd-only.
if [[ ! -e "$MNT/lib/systemd/systemd" && ! -e "$MNT/usr/lib/systemd/systemd" ]]; then
  die "base image is not systemd-based (no .../systemd/systemd); the guest unit would never start"
fi
ok "base confirmed systemd"

# Phase 4: the guest agent preens /dev/vdb with fsck before mounting /persist.
# The mount is a syscall (no `mount` binary needed) but fsck.ext4 (e2fsprogs)
# should be present, or persistence falls back to a no-fsck mount. Warn loudly
# if it is missing so the operator can add e2fsprogs to the base.
if [[ -x "$MNT/sbin/fsck.ext4" || -x "$MNT/usr/sbin/fsck.ext4" ]]; then
  ok "base ships fsck.ext4 (persist preen available)"
else
  log "WARNING: base image has no fsck.ext4 (e2fsprogs); /persist will mount without preen"
fi

log "installing guest agent + unit + release stamp"
sudo install -D -m 0755 "$GA_BIN" "$MNT/usr/local/bin/guestagent"
sudo install -D -m 0644 "$UNIT_SRC" "$MNT/etc/systemd/system/proteos-guestagent.service"
# Enable at boot by creating the multi-user.target.wants symlink (we can't run
# `systemctl enable` against an offline image, so do what it would do).
sudo mkdir -p "$MNT/etc/systemd/system/multi-user.target.wants"
sudo ln -sf ../proteos-guestagent.service \
  "$MNT/etc/systemd/system/multi-user.target.wants/proteos-guestagent.service"

# Phase 5: provider-secret wiring. The profile.d snippet sources the injected
# /run/proteos/env/*.env into login shells so provider CLIs see their keys; it is
# baked regardless of whether an agent CLI is installed (harmless no-op otherwise).
log "installing providers profile.d snippet"
sudo install -D -m 0644 "$PROFILE_SRC" "$MNT/etc/profile.d/proteos-providers.sh"

# Phase 5: optionally bake the pinned Claude Code agent CLI + its managed
# settings. The key itself is injected at runtime (never baked).
FEATURES="terminal,persist,resume,providers"
if [[ -n $CLAUDE_BIN ]]; then
  file "$CLAUDE_BIN" | grep -q "ELF" || die "claude binary is not an ELF executable: $CLAUDE_BIN"
  ACTUAL_SHA="$(sha256sum "$CLAUDE_BIN" | awk '{print $1}')"
  if [[ -n $CLAUDE_SHA256 ]]; then
    [[ "$CLAUDE_SHA256" == "$ACTUAL_SHA" ]] || die "claude sha256 mismatch: expected $CLAUDE_SHA256, got $ACTUAL_SHA"
    ok "claude sha256 verified ($ACTUAL_SHA)"
  else
    log "WARNING: --claude-sha256 not given; pinning to the binary's own sha256 ($ACTUAL_SHA)"
  fi
  CLAUDE_SHA256="$ACTUAL_SHA"
  log "installing Claude Code $CLAUDE_VERSION → /usr/local/bin/claude"
  sudo install -D -m 0755 "$CLAUDE_BIN" "$MNT/usr/local/bin/claude"
  # Fleet defaults (pin the version: no in-VM self-update of an immutable image).
  sudo install -D -m 0644 "$CLAUDE_SETTINGS_SRC" "$MNT/etc/claude-code/managed-settings.json"
  FEATURES="$FEATURES,claude"
  ok "Claude Code baked ($CLAUDE_VERSION)"
else
  log "WARNING: no --claude-binary; baking providers wiring but no agent CLI"
fi

# /etc/proteos-release — provenance the guest (and humans) can read.
BUILD_STAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)"
sudo tee "$MNT/etc/proteos-release" >/dev/null <<EOF
PROTEOS_ROOTFS_BASE=$BASE_NAME
PROTEOS_GUESTAGENT_VERSION=$VERSION
PROTEOS_GUESTAGENT_FEATURES=$FEATURES
PROTEOS_CLAUDE_VERSION=${CLAUDE_VERSION:-none}
PROTEOS_BUILD_AT=$BUILD_STAMP
EOF

sync
sudo umount "$MNT"
trap - EXIT
rm -rf "$WORK"
ok "image baked"

# --- 4. manifest.lock --------------------------------------------------------
SHA="$(sha256sum "$OUT_IMG" | awk '{print $1}')"
MANIFEST="$OUT_DIR/manifest.lock"
cat >"$MANIFEST" <<EOF
# Generated by image/build-rootfs.sh — commit this file.
# The control plane's PROTEOS_ROOTFS_REF / the per-machine rootfs_ref pin this image.
image          = $(basename "$OUT_IMG")
sha256         = $SHA
base_rootfs    = $BASE_NAME
guestagent     = $VERSION
features       = $FEATURES
claude_version = ${CLAUDE_VERSION:-none}
claude_sha256  = ${CLAUDE_SHA256:-none}
built_at       = $BUILD_STAMP
EOF
ok "wrote $MANIFEST"

log "done. Copy $OUT_IMG into the node-agent images dir and set its name as the"
log "machine rootfs_ref (PROTEOS_ROOTFS_REF) on the Proxmox host."
