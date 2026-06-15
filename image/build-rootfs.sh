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
# Phase 6: bake the three additional provider CLIs (Gemini, OpenAI Codex, pi.dev)
# alongside a Node LTS runtime. Like --claude-bootstrap, each is enabled with a
# flag and installs the LATEST version by default; pass --<name>-version to pin.
#
#   --gemini [--gemini-version X.Y.Z]   npm i -g @google/gemini-cli[@ver]
#   --codex  [--codex-version X.Y.Z]    npm i -g @openai/codex[@ver]
#   --pi     [--pi-version X.Y.Z]       npm i -g @pi/agent[@ver]  (pkg per PROVIDERS.md)
#   --node-version vX.Y.Z [--node-sha256 <hex>]
#       Pin the shared Node runtime. Omitted ⇒ the latest LTS is resolved from
#       nodejs.org. Node is installed automatically whenever any npm CLI above is
#       enabled (it is their runtime).
#
# A build with none of these still bakes the providers wiring + claude. The npm
# installs run via `chroot` into the mounted image (native arch, correct
# shebangs), so this must run on a host matching the rootfs arch with network.
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

# node_arch maps uname -m onto the nodejs.org dist arch tag.
node_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo "x64" ;;
    arm64 | aarch64) echo "arm64" ;;
    *) die "unsupported arch for Node: $(uname -m)" ;;
  esac
}

# install_node MNT VERSION [SHA256] — fetch the Node LTS tarball from nodejs.org,
# verify its sha256 (against the published SHASUMS256 if not given), and unpack it
# into the image's /usr/local (node + npm on PATH). A shared runtime for the
# npm-distributed agents (Phase 6 decision #4). An empty VERSION resolves the
# latest LTS (the --claude-bootstrap "latest by default" pattern).
install_node() {
  local mnt="$1" version="$2" want_sha="$3"
  local arch tarball url
  arch="$(node_arch)"

  # Resolve "latest LTS" from the canonical dist index.json when unpinned. It is
  # sorted newest-first, so the first entry whose "lts" is not false is the latest
  # LTS release.
  if [[ -z $version ]]; then
    log "resolving latest Node LTS"
    local index
    index="$(dl "https://nodejs.org/dist/index.json")" || die "fetch Node index.json"
    if command -v jq >/dev/null 2>&1; then
      version="$(printf '%s' "$index" | jq -r 'map(select(.lts != false))[0].version')"
    else
      # One object per line (objects start with '{'; nested arrays use '['), then
      # take the first with an "lts":"<name>" (false has no quote after the colon).
      version="$(printf '%s' "$index" | tr '{' '\n' \
        | grep -m1 '"lts":"' \
        | grep -oE '"version":"v[0-9]+\.[0-9]+\.[0-9]+"' \
        | grep -oE 'v[0-9.]+')"
    fi
    [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "could not resolve latest Node LTS (got '$version')"
    ok "latest Node LTS is $version"
  fi
  tarball="node-${version}-linux-${arch}.tar.xz"
  url="https://nodejs.org/dist/${version}/${tarball}"
  NODE_VERSION="$version"

  log "fetching Node ${version} (${arch})"
  dl "$url" "$WORK/$tarball" || die "download Node $version"

  local actual
  actual="$(sha256sum "$WORK/$tarball" | awk '{print $1}')"
  if [[ -z $want_sha ]]; then
    # Verify against the dist SHASUMS256.txt rather than trusting blindly.
    want_sha="$(dl "https://nodejs.org/dist/${version}/SHASUMS256.txt" | grep " ${tarball}\$" | awk '{print $1}')"
    [[ -n $want_sha ]] || die "no SHASUMS256 entry for $tarball"
  fi
  [[ "$actual" == "$want_sha" ]] || die "Node sha256 mismatch: expected $want_sha, got $actual"
  NODE_SHA256="$want_sha"
  ok "Node tarball sha256 verified ($actual)"

  # Unpack into /usr/local, stripping the top-level node-<ver> dir so binaries
  # land at /usr/local/bin/{node,npm,npx}.
  sudo tar -xJf "$WORK/$tarball" -C "$mnt/usr/local" --strip-components=1
  ok "Node ${version} unpacked into /usr/local"
}

# npm_global MNT SPEC — install an npm package globally INSIDE the image via
# chroot, so the package's native arch + #!/usr/bin/env node shebangs are correct
# at guest runtime. Requires /dev,/proc bind mounts (added by the caller) and the
# baked resolv.conf for registry DNS.
npm_global() {
  local mnt="$1" spec="$2"
  log "npm i -g $spec (chroot)"
  sudo chroot "$mnt" /usr/bin/env \
    PATH=/usr/local/bin:/usr/bin:/bin npm install -g --no-fund --no-audit "$spec" \
    || die "npm install -g $spec failed"
  ok "installed $spec"
}

# npm_spec PKG VERSION — "pkg@version" if pinned, else "pkg" (npm resolves latest).
npm_spec() {
  local pkg="$1" ver="$2"
  if [[ -n $ver ]]; then echo "${pkg}@${ver}"; else echo "$pkg"; fi
}

# npm_resolved MNT PKG — echo the concrete globally-installed version of PKG, so a
# "latest" install records its real version in manifest.lock / PROVIDERS.md.
npm_resolved() {
  sudo chroot "$1" /usr/bin/env PATH=/usr/local/bin:/usr/bin:/bin \
    npm ls -g "$2" --depth=0 2>/dev/null \
    | grep -oE "${2}@[0-9][0-9A-Za-z.-]*" | sed -E 's/.*@//' | head -1
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
# Phase 6 provider pins. Each CLI is off unless its --<name> flag (or --<name>-
# version) is given; an empty version installs the latest. Default off → behaviour
# identical to Phase 5.
NODE_VERSION=""
NODE_SHA256=""
GEMINI=0
GEMINI_VERSION=""
CODEX=0
CODEX_VERSION=""
PI=0
PI_VERSION=""
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
    --node-version) NODE_VERSION=$2; shift 2 ;;
    --node-sha256) NODE_SHA256=$2; shift 2 ;;
    --gemini) GEMINI=1; shift ;;
    --gemini-version) GEMINI=1; GEMINI_VERSION=$2; shift 2 ;;
    --codex) CODEX=1; shift ;;
    --codex-version) CODEX=1; CODEX_VERSION=$2; shift 2 ;;
    --pi) PI=1; shift ;;
    --pi-version) PI=1; PI_VERSION=$2; shift 2 ;;
    *) die "unknown arg: $1" ;;
  esac
done
# Any npm-distributed CLI implies the Node runtime.
NEED_NODE=0
[[ $GEMINI -eq 1 || $CODEX -eq 1 || $PI -eq 1 ]] && NEED_NODE=1
[[ -n $BASE ]] || die "--base <pinned firecracker-ci ext4> is required"
[[ -f $BASE ]] || die "base rootfs not found: $BASE"
if [[ -n $CLAUDE_BIN ]]; then
  [[ $CLAUDE_BOOTSTRAP -eq 0 ]] || die "use either --claude-binary or --claude-bootstrap, not both"
  [[ -f $CLAUDE_BIN ]] || die "claude binary not found: $CLAUDE_BIN"
  [[ -n $CLAUDE_VERSION ]] || die "--claude-version is required with --claude-binary (pins the manifest)"
fi
# Phase 6: the npm CLIs share the Node runtime; if any is enabled without a pinned
# Node version, the latest LTS is resolved at install time (see install_node).

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
# Phase 6: Node (~120MiB unpacked) + the npm CLIs grow the image materially.
# Reserve generous headroom so the chroot npm installs don't ENOSPC; the exact
# final size is recorded in PROVIDERS.md after a real bake.
if [[ $NEED_NODE -eq 1 || -n $NODE_VERSION ]]; then
  PROV_NEED=$((GROW_MIB + 512))
  log "baking Node/provider CLIs — bumping grow ${GROW_MIB}→${PROV_NEED}MiB headroom"
  GROW_MIB=$PROV_NEED
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
  # Release any chroot binds (Phase 6 npm install) before the image umount, or
  # $MNT is busy. Defensive even if a bind failed midway.
  sudo umount "$MNT/sys" 2>/dev/null || true
  sudo umount "$MNT/proc" 2>/dev/null || true
  sudo umount "$MNT/dev" 2>/dev/null || true
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

# The guest gets its IP from the kernel `ip=` cmdline (see firecracker.go), which
# sets the address/gateway but NO DNS resolver, and there is no DHCP or
# systemd-resolved managing the static NIC. Without a resolver every lookup fails
# ("Could not resolve host") and agent CLIs can't reach their APIs. Bake a static
# /etc/resolv.conf, replacing the CI image's symlink-to-stub so it actually holds
# nameservers. Egress is NATed to the internet by the node-agent's nft rules.
log "installing static resolv.conf (kernel ip= sets no DNS)"
sudo rm -f "$MNT/etc/resolv.conf"
sudo tee "$MNT/etc/resolv.conf" >/dev/null <<'EOF'
nameserver 1.1.1.1
nameserver 8.8.8.8
EOF

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

# Phase 6: bake the Node runtime + the Gemini/Codex/pi.dev CLIs (all npm). Auth
# for all three is injected at runtime (Gemini/Pi via env; Codex via the registry
# setup_command login) — nothing secret is baked. Each installs LATEST unless a
# version was pinned. See image/PROVIDERS.md.
CHROOT_BOUND=0
bind_chroot() {
  # npm postinstalls and the dynamic loader expect /dev,/proc,/sys present.
  sudo mount --bind /dev "$MNT/dev"
  sudo mount --bind /proc "$MNT/proc"
  sudo mount --bind /sys "$MNT/sys"
  CHROOT_BOUND=1
}
unbind_chroot() {
  [[ $CHROOT_BOUND -eq 1 ]] || return 0
  sudo umount "$MNT/sys" 2>/dev/null || true
  sudo umount "$MNT/proc" 2>/dev/null || true
  sudo umount "$MNT/dev" 2>/dev/null || true
  CHROOT_BOUND=0
}

if [[ $NEED_NODE -eq 1 || -n $NODE_VERSION ]]; then
  install_node "$MNT" "$NODE_VERSION" "$NODE_SHA256"   # resolves latest LTS if unpinned
  FEATURES="$FEATURES,node"

  # Bind /dev,/proc,/sys so the chroot npm installs work; release on any exit
  # path (the EXIT trap would otherwise try to umount the busy image).
  bind_chroot
  if [[ $GEMINI -eq 1 ]]; then
    npm_global "$MNT" "$(npm_spec @google/gemini-cli "$GEMINI_VERSION")"
    GEMINI_VERSION="$(npm_resolved "$MNT" @google/gemini-cli)"
    FEATURES="$FEATURES,gemini"
  fi
  if [[ $CODEX -eq 1 ]]; then
    npm_global "$MNT" "$(npm_spec @openai/codex "$CODEX_VERSION")"
    CODEX_VERSION="$(npm_resolved "$MNT" @openai/codex)"
    FEATURES="$FEATURES,codex"
  fi
  if [[ $PI -eq 1 ]]; then
    # The pi.dev coding agent: its bin is `pi` (matches the registry launch
    # command). NOT @oh-my-pi/pi-coding-agent, which ships an `omp` binary.
    npm_global "$MNT" "$(npm_spec @earendil-works/pi-coding-agent "$PI_VERSION")"
    PI_VERSION="$(npm_resolved "$MNT" @earendil-works/pi-coding-agent)"
    FEATURES="$FEATURES,pi"
  fi
  unbind_chroot
fi

# /etc/proteos-release — provenance the guest (and humans) can read.
BUILD_STAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)"
sudo tee "$MNT/etc/proteos-release" >/dev/null <<EOF
PROTEOS_ROOTFS_BASE=$BASE_NAME
PROTEOS_GUESTAGENT_VERSION=$VERSION
PROTEOS_GUESTAGENT_FEATURES=$FEATURES
PROTEOS_CLAUDE_VERSION=${CLAUDE_VERSION:-none}
PROTEOS_NODE_VERSION=${NODE_VERSION:-none}
PROTEOS_GEMINI_VERSION=${GEMINI_VERSION:-none}
PROTEOS_CODEX_VERSION=${CODEX_VERSION:-none}
PROTEOS_PI_VERSION=${PI_VERSION:-none}
PROTEOS_BUILD_AT=$BUILD_STAMP
EOF

sync
sudo umount "$MNT"
trap - EXIT
rm -rf "$WORK"
ok "image baked"

# --- 4. manifest.lock --------------------------------------------------------
SHA="$(sha256sum "$OUT_IMG" | awk '{print $1}')"
# Capture provenance the bake-report can surface without re-measuring: the final
# image size (MiB) and the wall-clock build duration ($SECONDS counts from script
# start). Both answer the "how big / how long" questions PROVIDERS.md asks.
IMAGE_SIZE_MIB="$(du -m "$OUT_IMG" | awk '{print $1}')"
BUILD_SECONDS=$SECONDS
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
node_version   = ${NODE_VERSION:-none}
node_sha256    = ${NODE_SHA256:-none}
gemini_version = ${GEMINI_VERSION:-none}
codex_version  = ${CODEX_VERSION:-none}
pi_version     = ${PI_VERSION:-none}
image_size_mib = ${IMAGE_SIZE_MIB}
build_seconds  = ${BUILD_SECONDS}
built_at       = $BUILD_STAMP
EOF
ok "wrote $MANIFEST (${IMAGE_SIZE_MIB} MiB, ${BUILD_SECONDS}s)"

log "done. Copy $OUT_IMG into the node-agent images dir and set its name as the"
log "machine rootfs_ref (PROTEOS_ROOTFS_REF) on the Proxmox host."
