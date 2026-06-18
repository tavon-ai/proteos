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
# Phase 7: git is installed into the guest by default (clone/commit/push need a
# real git binary; the ProteOS credential helper is the guestagent binary, wired
# via gitconfig at runtime — nothing secret is baked). It is installed via apt in
# the chroot and is idempotent (skipped if the base already ships git). Pass
# --no-git to skip (air-gapped builds, or a base that already includes git).
#
# Guest dev tooling: vim, the Go toolchain, and the Taskfile (`task`) CLI are
# baked into the guest by default (--no-vim / --no-go / --no-taskfile opt out).
# vim goes in extract-only via apt (like git); Go is unpacked into /usr/local/go
# and put on PATH; Taskfile installs `task` onto /usr/local/bin. Pin versions with
# --go-version X.Y.Z / --taskfile-version vX.Y.Z.
#
#   --alias 'name=command'   bake a shell alias into the guest's interactive
#                            shells (root + run-as user); repeatable.
#   --bashrc-file FILE       append FILE's contents to the managed bashrc snippet;
#                            repeatable. Both land in /etc/profile.d/proteos-shell.sh
#                            and are sourced from the relevant .bashrc files.
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

# cs_arch maps uname -m onto the code-server release arch tag.
cs_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo "amd64" ;;
    arm64 | aarch64) echo "arm64" ;;
    *) die "unsupported arch for code-server: $(uname -m)" ;;
  esac
}

# install_codeserver MNT VERSION [SHA256] — fetch the pinned code-server
# STANDALONE release tarball from GitHub (it bundles its own Node, so it needs no
# system Node and stays self-contained — Phase 8 decision #5), verify its sha256,
# and unpack it into /usr/local/lib/code-server with /usr/local/bin/code-server
# symlinked to its entrypoint. An empty VERSION resolves the latest GitHub release.
# The editor binds loopback only and is started lazily by the guest agent's web
# forward; nothing here wires auth (the gateway authenticates). Sets CS_VERSION.
install_codeserver() {
  local mnt="$1" version="$2" want_sha="$3"
  local arch tarball url
  arch="$(cs_arch)"

  if [[ -z $version ]]; then
    log "resolving latest code-server release"
    local tag
    tag="$(dl https://api.github.com/repos/coder/code-server/releases/latest \
      | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"v[0-9]+\.[0-9]+\.[0-9]+"' \
      | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | head -1)" || die "resolve code-server latest"
    [[ -n $tag ]] || die "could not resolve latest code-server release"
    version="${tag#v}"
    ok "latest code-server is $version"
  fi
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+ ]] || die "bad code-server version: $version"

  tarball="code-server-${version}-linux-${arch}.tar.gz"
  url="https://github.com/coder/code-server/releases/download/v${version}/${tarball}"
  log "fetching code-server ${version} (${arch})"
  dl "$url" "$WORK/$tarball" || die "download code-server $version"

  local actual
  actual="$(sha256sum "$WORK/$tarball" | awk '{print $1}')"
  if [[ -n $want_sha ]]; then
    [[ "$actual" == "$want_sha" ]] || die "code-server sha256 mismatch: expected $want_sha, got $actual"
    ok "code-server sha256 verified ($actual)"
  else
    log "WARNING: --codeserver-sha256 not given; pinning to the tarball's own sha256 ($actual)"
  fi
  CS_SHA256="$actual"

  # Unpack into /usr/local/lib/code-server (strip the top-level versioned dir),
  # then symlink the entrypoint onto PATH.
  sudo rm -rf "$mnt/usr/local/lib/code-server"
  sudo mkdir -p "$mnt/usr/local/lib/code-server"
  sudo tar -xzf "$WORK/$tarball" -C "$mnt/usr/local/lib/code-server" --strip-components=1
  sudo ln -sf ../lib/code-server/bin/code-server "$mnt/usr/local/bin/code-server"
  CS_VERSION="$version"
  ok "code-server ${version} installed (/usr/local/bin/code-server)"
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

# install_git MNT — ensure git is present in the image (Phase 7). The guest needs
# a real git binary for clone/commit/push; the ProteOS credential helper is the
# guestagent binary, wired via gitconfig at runtime (no secret is baked).
#
# The firecracker-CI base is slimmed and carries NO dpkg metadata, so a plain
# `apt-get install git` thinks nothing is installed and reinstalls git's entire
# dependency closure — libc6, perl, dpkg, tar … — over files that already exist,
# running every maintainer script (ldconfig, systemd timers, triggers). That
# perturbation destabilises the image and breaks the Phase 4 hibernate/resume
# cycle (observed on the KVM gate). So instead we let apt only DOWNLOAD the
# closure, then lay down — with `dpkg-deb -x`, running no scripts — only the
# packages whose files are genuinely missing from the base. Idempotent: if the
# base already ships git it records the version and returns. Sets GIT_VERSION.
install_git() {
  local mnt="$1"
  if sudo chroot "$mnt" /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin sh -c 'command -v git' >/dev/null 2>&1; then
    GIT_VERSION="$(sudo chroot "$mnt" /usr/bin/env PATH=/usr/local/bin:/usr/bin:/bin git --version 2>/dev/null | awk '{print $3}')"
    ok "git already present in base (${GIT_VERSION:-unknown})"
    return
  fi
  command -v dpkg-deb >/dev/null || die "dpkg-deb required on the build host to lay down git"

  log "installing git (apt download + extract-only; avoids reinstalling base packages)"
  # apt drops privileges to the _apt sandbox user to fetch, but in a loop-mounted
  # chroot that user often cannot create temp files under /tmp — which breaks
  # apt-key and makes every repo look "not signed". Disable the sandbox so apt
  # runs as root, and recreate the apt cache/list/log dirs the slimmed base
  # stripped (else apt fails on a missing ".../partial" or "/var/log/apt/").
  local apt_opts='-o APT::Sandbox::User=root -o Acquire::Retries=3'
  sudo chroot "$mnt" /usr/bin/env \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive \
    sh -c "mkdir -p /tmp /var/log /var/log/apt /var/cache/apt/archives/partial /var/lib/apt/lists/partial && \
      apt-get $apt_opts update -qq && apt-get $apt_opts -d install -y --no-install-recommends git ca-certificates" \
    || die "apt-get download of git failed (the base image needs working apt sources + network; set proteos_git_install=false / pass --no-git to skip)"

  # Lay down only the missing packages, running no maintainer scripts.
  local cache="$mnt/var/cache/apt/archives" laid=0
  for deb in "$cache"/*.deb; do
    [[ -e $deb ]] || continue
    # Core packages that are guaranteed present in any bootable base and must
    # never be overwritten (their reinstall is what breaks resume).
    case "$(basename "$deb")" in
      libc6_* | libc-bin_* | libcrypt1_* | libgcc-s1_* | gcc-*-base_* | \
        perl_* | perl-base_* | perl-modules-* | debconf_* | dpkg_* | tar_*)
        continue
        ;;
    esac
    # For the rest, probe a binary/library the package ships; if it already
    # exists in the base the package is effectively present, so skip it. The
    # trailing `|| true` keeps `set -o pipefail` from aborting when grep finds no
    # lib/bin file (doc-only packages) or head closes the pipe early (SIGPIPE).
    local probe
    # `head -1` closes the pipe early, so dpkg-deb's tar child takes a SIGPIPE
    # and whines "tar subprocess was killed by signal (Broken pipe)" — harmless
    # (we only want the first matching path); drop its stderr to keep logs clean.
    probe="$(dpkg-deb -c "$deb" 2>/dev/null | awk '$1 ~ /^-/ {print $NF}' | grep -E '^\./(usr/)?(lib|lib64|bin|sbin)/' | head -1 || true)"
    probe="${probe#.}"
    if [[ -n $probe && -e "$mnt$probe" ]]; then
      continue
    fi
    sudo dpkg-deb -x "$deb" "$mnt"
    laid=$((laid + 1))
  done
  sudo rm -f "$cache"/*.deb
  log "git: laid down $laid package(s) missing from the base"

  # ca-certificates extract-only ships the cert sources but not the merged bundle
  # git uses for HTTPS; build it once (touches only /etc/ssl/certs, no scripts).
  if [[ -x "$mnt/usr/sbin/update-ca-certificates" && ! -s "$mnt/etc/ssl/certs/ca-certificates.crt" ]]; then
    sudo chroot "$mnt" /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
      update-ca-certificates >/dev/null 2>&1 || true
  fi

  GIT_VERSION="$(sudo chroot "$mnt" /usr/bin/env PATH=/usr/local/bin:/usr/bin:/bin git --version 2>/dev/null | awk '{print $3}' || true)"
  [[ -n $GIT_VERSION ]] || die "git extract-only install failed: git not runnable in the image (try a base that ships git, or proteos_git_install=false)"
  ok "git installed (${GIT_VERSION})"
}

# lay_down_cache MNT — extract every .deb currently in the image's apt archive
# cache with `dpkg-deb -x` (no maintainer scripts), skipping core packages that
# any bootable base already ships (reinstalling those is what breaks Phase 4
# resume — see install_git). Same extract-only discipline; factored out so the
# user-provisioning step can reuse it without perturbing install_git.
lay_down_cache() {
  local mnt="$1" cache="$1/var/cache/apt/archives" laid=0
  for deb in "$cache"/*.deb; do
    [[ -e $deb ]] || continue
    case "$(basename "$deb")" in
      libc6_* | libc-bin_* | libcrypt1_* | libgcc-s1_* | gcc-*-base_* | \
        perl_* | perl-base_* | perl-modules-* | debconf_* | dpkg_* | tar_*)
        continue
        ;;
    esac
    local probe
    # `head -1` closes the pipe early, so dpkg-deb's tar child takes a SIGPIPE
    # and whines "tar subprocess was killed by signal (Broken pipe)" — harmless
    # (we only want the first matching path); drop its stderr to keep logs clean.
    probe="$(dpkg-deb -c "$deb" 2>/dev/null | awk '$1 ~ /^-/ {print $NF}' | grep -E '^\./(usr/)?(lib|lib64|bin|sbin)/' | head -1 || true)"
    probe="${probe#.}"
    if [[ -n $probe && -e "$mnt$probe" ]]; then
      continue
    fi
    sudo dpkg-deb -x "$deb" "$mnt"
    laid=$((laid + 1))
  done
  sudo rm -f "$cache"/*.deb
  echo "$laid"
}

# provision_user MNT USER UID — create the unprivileged login the guest runs its
# shells + agent CLIs as (Phase 8). The guest agent stays root, but every PTY it
# spawns drops to this user, so a rogue agent command is non-root and tools (npm,
# git, installers) behave the way they do on a real workstation. Grants
# passwordless sudo (the microVM is the real isolation boundary; the non-root
# default is realism + a speed-bump). Idempotent: skips the user if it exists and
# only installs sudo if missing. Needs the chroot binds (apt). Sets USER_NAME.
provision_user() {
  local mnt="$1" user="$2"
  command -v dpkg-deb >/dev/null || die "dpkg-deb required on the build host to lay down sudo"
  sudo chroot "$mnt" /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    sh -c 'command -v useradd' >/dev/null 2>&1 \
    || die "base image lacks useradd (the 'passwd' package); cannot create the '$user' user (pass --no-user to skip)"

  # sudo, extract-only (apt would reinstall the base closure in the slimmed image
  # and break resume — same reasoning as install_git).
  if sudo chroot "$mnt" /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin sh -c 'command -v sudo' >/dev/null 2>&1; then
    ok "sudo already present in base"
  else
    log "installing sudo (apt download + extract-only)"
    local apt_opts='-o APT::Sandbox::User=root -o Acquire::Retries=3'
    sudo chroot "$mnt" /usr/bin/env \
      PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive \
      sh -c "mkdir -p /tmp /var/log /var/log/apt /var/cache/apt/archives/partial /var/lib/apt/lists/partial && \
        apt-get $apt_opts update -qq && apt-get $apt_opts -d install -y --no-install-recommends sudo" \
      || die "apt-get download of sudo failed (base needs working apt sources + network; pass --no-user to skip)"
    local laid
    laid="$(lay_down_cache "$mnt")"
    log "sudo: laid down $laid package(s) missing from the base"
    sudo chroot "$mnt" /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin sh -c 'command -v sudo' >/dev/null 2>&1 \
      || die "sudo extract-only install failed: sudo not runnable in the image"
  fi

  # Create the user idempotently (-m seeds /home/$user from /etc/skel; useradd
  # also makes a primary group named after the user — USERGROUPS_ENAB on Ubuntu).
  # We do NOT pin uid/gid: the cloud base already occupies 1000 (its default
  # 'ubuntu' user), so let useradd take the next free id. The guest agent resolves
  # this user by NAME (not a hardcoded number), so the exact id does not matter.
  sudo chroot "$mnt" /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    sh -c "id -u $user >/dev/null 2>&1 || useradd -m -s /bin/bash $user" \
    || die "useradd $user failed"
  local uid
  uid="$(sudo chroot "$mnt" /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin id -u "$user" 2>/dev/null)"

  # Passwordless sudo for the user (0440, root-owned, as visudo requires).
  printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$user" | sudo tee "$mnt/etc/sudoers.d/$user" >/dev/null
  sudo chmod 0440 "$mnt/etc/sudoers.d/$user"
  sudo chown 0:0 "$mnt/etc/sudoers.d/$user"

  USER_NAME="$user"
  ok "provisioned unprivileged user '$user' (uid ${uid:-?}) with passwordless sudo"
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

# apt_extract_install MNT PKG... — download the named apt packages (+ their
# missing deps) inside the chroot, then lay them down extract-only with
# `dpkg-deb -x` (running NO maintainer scripts) — the same resume-safe discipline
# install_git/provision_user use, because the slimmed firecracker-CI base has no
# dpkg metadata and a normal install would reinstall the base closure and break
# Phase 4 resume. Needs the chroot binds (caller brackets bind_chroot) + network.
apt_extract_install() {
  local mnt="$1"
  shift
  local pkgs="$*"
  command -v dpkg-deb >/dev/null || die "dpkg-deb required on the build host to lay down: $pkgs"
  log "installing $pkgs (apt download + extract-only)"
  local apt_opts='-o APT::Sandbox::User=root -o Acquire::Retries=3'
  sudo chroot "$mnt" /usr/bin/env \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive \
    sh -c "mkdir -p /tmp /var/log /var/log/apt /var/cache/apt/archives/partial /var/lib/apt/lists/partial && \
      apt-get $apt_opts update -qq && apt-get $apt_opts -d install -y --no-install-recommends $pkgs" \
    || die "apt-get download of '$pkgs' failed (base needs working apt sources + network)"
  local laid
  laid="$(lay_down_cache "$mnt")"
  log "$pkgs: laid down $laid package(s) missing from the base"
}

# install_vim MNT — install vim into the guest (extract-only, resume-safe).
# Idempotent: skips if the base already ships vim.
install_vim() {
  local mnt="$1"
  if sudo chroot "$mnt" /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin sh -c 'command -v vim' >/dev/null 2>&1; then
    ok "vim already present in base"
    return
  fi
  apt_extract_install "$mnt" vim
  # The vim package ships /usr/bin/vim.basic and relies on update-alternatives
  # (run from its postinst) to create the /usr/bin/vim -> vim.basic symlink. We
  # install extract-only and run NO maintainer scripts, so that symlink never
  # appears — lay it down ourselves (same for the `vi`/`editor` alternatives so
  # the usual entrypoints all work). Unlike git/sudo, vim has no binary at the
  # canonical path without this.
  if ! sudo chroot "$mnt" /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin sh -c 'command -v vim' >/dev/null 2>&1; then
    [[ -e "$mnt/usr/bin/vim.basic" ]] \
      || die "vim extract-only install failed: /usr/bin/vim.basic not laid down (vim deb or its deps did not extract)"
    sudo chroot "$mnt" /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
      sh -c 'for link in vim vi editor; do ln -sf /usr/bin/vim.basic /usr/bin/$link; done'
  fi
  sudo chroot "$mnt" /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin sh -c 'command -v vim' >/dev/null 2>&1 \
    || die "vim extract-only install failed: vim not runnable in the image"
  ok "vim installed"
}

# go_arch maps uname -m onto the go.dev dist arch tag.
go_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo "amd64" ;;
    arm64 | aarch64) echo "arm64" ;;
    *) die "unsupported arch for Go: $(uname -m)" ;;
  esac
}

# install_go MNT VERSION — fetch the pinned Go toolchain tarball from go.dev,
# verify its sha256 against the published <tarball>.sha256, and unpack it into the
# image's /usr/local/go (the tarball's own top-level dir). Go is put on PATH for
# guest shells by install_shell_env. Sets GO_SHA256.
install_go() {
  local mnt="$1" version="$2"
  local arch tarball url
  arch="$(go_arch)"
  tarball="go${version}.linux-${arch}.tar.gz"
  url="https://go.dev/dl/${tarball}"
  log "fetching Go ${version} (${arch})"
  dl "$url" "$WORK/$tarball" || die "download Go $version"
  local actual want
  actual="$(sha256sum "$WORK/$tarball" | awk '{print $1}')"
  # go.dev does NOT serve detached <tarball>.sha256 files — that path returns the
  # site's HTML shell with HTTP 200 (so curl -f does not catch it, and the page
  # gets mistaken for a checksum). The published sha256 lives in the JSON index;
  # collapse it to one line and pull the sha256 from the same object as our file.
  want="$(dl "https://go.dev/dl/?mode=json&include=all" 2>/dev/null | tr -d '\n ' \
    | grep -oE "\"filename\":\"${tarball//./\\.}\"[^}]*\"sha256\":\"[0-9a-f]{64}\"" \
    | grep -oE '[0-9a-f]{64}' | head -1)" || true
  if [[ -n $want ]]; then
    [[ "$actual" == "$want" ]] || die "Go sha256 mismatch: expected $want, got $actual"
    ok "Go tarball sha256 verified ($actual)"
  else
    log "WARNING: could not fetch Go checksum; pinning to the tarball's own sha256 ($actual)"
  fi
  GO_SHA256="$actual"
  sudo rm -rf "$mnt/usr/local/go"
  sudo tar -xzf "$WORK/$tarball" -C "$mnt/usr/local"
  ok "Go ${version} unpacked into /usr/local/go"
}

# install_taskfile MNT VERSION — fetch the pinned go-task release tarball from
# GitHub (VERSION is a tag like v3.40.0), verify its sha256 against the published
# task_checksums.txt, and install the `task` binary onto /usr/local/bin. Sets
# TASK_SHA256.
install_taskfile() {
  local mnt="$1" version="$2"
  local arch tarball url
  arch="$(go_arch)"   # go-task uses the same amd64/arm64 tags as Go
  tarball="task_linux_${arch}.tar.gz"
  url="https://github.com/go-task/task/releases/download/${version}/${tarball}"
  log "fetching Taskfile ${version} (${arch})"
  dl "$url" "$WORK/$tarball" || die "download Taskfile $version"
  local actual want
  actual="$(sha256sum "$WORK/$tarball" | awk '{print $1}')"
  want="$(dl "https://github.com/go-task/task/releases/download/${version}/task_checksums.txt" 2>/dev/null \
    | grep " ${tarball}\$" | awk '{print $1}')" || true
  if [[ -n $want ]]; then
    [[ "$actual" == "$want" ]] || die "Taskfile sha256 mismatch: expected $want, got $actual"
    ok "Taskfile tarball sha256 verified ($actual)"
  else
    log "WARNING: could not fetch Taskfile checksums; pinning to the tarball's own sha256 ($actual)"
  fi
  TASK_SHA256="$actual"
  local tmp="$WORK/taskfile"
  mkdir -p "$tmp"
  tar -xzf "$WORK/$tarball" -C "$tmp"
  sudo install -D -m 0755 "$tmp/task" "$mnt/usr/local/bin/task"
  ok "Taskfile ${version} installed (/usr/local/bin/task)"
}

# emit_alias NAME=VALUE — print a shell `alias NAME='VALUE'` line, single-quoting
# the value (embedded single-quotes escaped) so spaces/metachars survive.
emit_alias() {
  local spec="$1" name val q="'"
  name="${spec%%=*}"
  val="${spec#*=}"
  val="${val//$q/$q\\$q$q}"   # replace each ' with the '\'' close-escape-reopen idiom
  printf "alias %s='%s'\n" "$name" "$val"
}

# install_shell_env MNT — bake a managed shell snippet (Go on PATH + operator
# aliases / appended bashrc files) into /etc/profile.d AND source it from the
# interactive bashrc of root, /etc/skel, and the provisioned run-as user.
# profile.d covers login shells; the bashrc include covers non-login interactive
# shells (the ttyd PTYs the guest spawns). Idempotent via a marker line.
install_shell_env() {
  local mnt="$1"
  local snip="$WORK/proteos-shell.sh"
  {
    echo "# Generated by image/build-rootfs.sh — do not edit (regenerated each bake)."
    if [[ $GO_INSTALL -eq 1 ]]; then
      echo '# Go toolchain on PATH (baked into /usr/local/go).'
      echo 'export PATH="$PATH:/usr/local/go/bin:${GOPATH:-$HOME/go}/bin"'
    fi
    if [[ ${#ALIASES[@]} -gt 0 ]]; then
      echo '# Operator aliases (--alias).'
      local a
      for a in "${ALIASES[@]}"; do emit_alias "$a"; done
    fi
    if [[ ${#BASHRC_FILES[@]} -gt 0 ]]; then
      local f
      for f in "${BASHRC_FILES[@]}"; do
        echo "# --- appended from $(basename "$f") (--bashrc-file) ---"
        cat "$f"
      done
    fi
  } >"$snip"
  sudo install -D -m 0644 "$snip" "$mnt/etc/profile.d/proteos-shell.sh"
  ok "installed /etc/profile.d/proteos-shell.sh (Go PATH + aliases)"

  local marker="# proteos-managed shell snippet"
  local rcs=("$mnt/root/.bashrc" "$mnt/etc/skel/.bashrc")
  [[ $USER_PROVISION -eq 1 ]] && rcs+=("$mnt/home/$RUN_AS_USER/.bashrc")
  local rc
  for rc in "${rcs[@]}"; do
    sudo mkdir -p "$(dirname "$rc")"
    [[ -e $rc ]] || sudo touch "$rc"
    if sudo grep -qF "$marker" "$rc" 2>/dev/null; then continue; fi
    printf '\n%s (aliases + Go PATH)\n[ -f /etc/profile.d/proteos-shell.sh ] && . /etc/profile.d/proteos-shell.sh\n' \
      "$marker" | sudo tee -a "$rc" >/dev/null
  done
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
# Phase 7: git in the guest. On by default — clone/commit/push need it and the
# credential helper is wired at runtime. --no-git opts out (e.g. a base that
# already ships git, or an air-gapped build with no apt).
GIT_INSTALL=1
GIT_VERSION=""
# Phase 8: provision an unprivileged 'dev' user (uid 1000) the guest runs its
# shells + agent CLIs as, with passwordless sudo. --no-user opts out (sessions
# then run as root, the legacy behavior — the guest agent falls back to root when
# the user is absent).
USER_PROVISION=1
USER_NAME=""
RUN_AS_USER="dev"
# Phase 8: bake the code-server editor (standalone release, self-contained). On
# by default — the browser editor is reached through the guest agent's web
# forward (vsock:1025). --no-codeserver opts out (air-gapped builds, or a base
# that already ships it). Installs the LATEST release unless --codeserver-version
# pins one; --codeserver-sha256 verifies the tarball.
CODESERVER_INSTALL=1
CODESERVER_VERSION=""
CODESERVER_SHA256=""
CS_VERSION=""
CS_SHA256=""
# Guest dev tooling: vim, the Go toolchain (unpacked into /usr/local/go and put
# on PATH), and the Taskfile (`task`) CLI. All on by default — the operator wants
# them on every machine; --no-vim/--no-go/--no-taskfile opt out. Versions are
# pinned (Go mirrors the host toolchain pin; bump with --go-version/--taskfile-version).
VIM_INSTALL=1
GO_INSTALL=1
GO_VERSION="1.26.4"
GO_SHA256=""
TASKFILE_INSTALL=1
TASKFILE_VERSION="v3.40.0"
TASK_SHA256=""
# Operator shell customisation baked into the guest's interactive shells: aliases
# (--alias 'name=command', repeatable) and/or whole files appended to the managed
# bashrc snippet (--bashrc-file FILE, repeatable).
ALIASES=()
BASHRC_FILES=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --base) BASE=$2; shift 2 ;;
    --git) GIT_INSTALL=1; shift ;;
    --no-git) GIT_INSTALL=0; shift ;;
    --user) USER_PROVISION=1; shift ;;
    --no-user) USER_PROVISION=0; shift ;;
    --codeserver) CODESERVER_INSTALL=1; shift ;;
    --no-codeserver) CODESERVER_INSTALL=0; shift ;;
    --codeserver-version) CODESERVER_VERSION=$2; shift 2 ;;
    --codeserver-sha256) CODESERVER_SHA256=$2; shift 2 ;;
    --vim) VIM_INSTALL=1; shift ;;
    --no-vim) VIM_INSTALL=0; shift ;;
    --go) GO_INSTALL=1; shift ;;
    --no-go) GO_INSTALL=0; shift ;;
    --go-version) GO_INSTALL=1; GO_VERSION=$2; shift 2 ;;
    --taskfile) TASKFILE_INSTALL=1; shift ;;
    --no-taskfile) TASKFILE_INSTALL=0; shift ;;
    --taskfile-version) TASKFILE_INSTALL=1; TASKFILE_VERSION=$2; shift 2 ;;
    --alias) ALIASES+=("$2"); shift 2 ;;
    --bashrc-file) [[ -f $2 ]] || die "--bashrc-file not found: $2"; BASHRC_FILES+=("$2"); shift 2 ;;
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
# Phase 7: git + its apt dependencies need a little headroom when not in the base.
if [[ $GIT_INSTALL -eq 1 ]]; then
  GIT_NEED=$((GROW_MIB + 192))
  log "baking git — bumping grow ${GROW_MIB}→${GIT_NEED}MiB headroom"
  GROW_MIB=$GIT_NEED
fi
# Phase 8: code-server (standalone, bundles Node) unpacks to ~350MiB.
if [[ $CODESERVER_INSTALL -eq 1 ]]; then
  CS_NEED=$((GROW_MIB + 450))
  log "baking code-server — bumping grow ${GROW_MIB}→${CS_NEED}MiB headroom"
  GROW_MIB=$CS_NEED
fi
# Guest dev tooling: vim's apt closure, the Go SDK (~600MiB extracted), Taskfile.
if [[ $VIM_INSTALL -eq 1 ]]; then
  VIM_NEED=$((GROW_MIB + 96))
  log "baking vim — bumping grow ${GROW_MIB}→${VIM_NEED}MiB headroom"
  GROW_MIB=$VIM_NEED
fi
if [[ $GO_INSTALL -eq 1 ]]; then
  GO_NEED=$((GROW_MIB + 700))
  log "baking Go — bumping grow ${GROW_MIB}→${GO_NEED}MiB headroom"
  GROW_MIB=$GO_NEED
fi
if [[ $TASKFILE_INSTALL -eq 1 ]]; then
  TASK_NEED=$((GROW_MIB + 32))
  log "baking Taskfile — bumping grow ${GROW_MIB}→${TASK_NEED}MiB headroom"
  GROW_MIB=$TASK_NEED
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

# Phase 7: ensure git is in the guest (idempotent). Needs the chroot binds for
# apt, so do it inside a bind/unbind even when no npm CLIs are baked.
if [[ $GIT_INSTALL -eq 1 ]]; then
  bind_chroot
  install_git "$MNT"
  unbind_chroot
  FEATURES="$FEATURES,git"
fi

# Phase 8: provision the unprivileged 'dev' user (+ sudo). Needs the chroot binds
# for apt (sudo) and useradd, so bracket it the same way.
if [[ $USER_PROVISION -eq 1 ]]; then
  bind_chroot
  provision_user "$MNT" "$RUN_AS_USER"
  unbind_chroot
  FEATURES="$FEATURES,user"
fi

# Phase 8: bake code-server (decision #5). The standalone tarball is
# self-contained (bundles its own Node), so it needs no chroot and is just
# unpacked into /usr/local/lib + symlinked onto PATH.
if [[ $CODESERVER_INSTALL -eq 1 ]]; then
  install_codeserver "$MNT" "$CODESERVER_VERSION" "$CODESERVER_SHA256"
  FEATURES="$FEATURES,codeserver"
fi

# Guest dev tooling. vim goes in via apt (needs the chroot binds); Go + Taskfile
# are self-contained tarballs unpacked onto /usr/local (no chroot needed).
if [[ $VIM_INSTALL -eq 1 ]]; then
  bind_chroot
  install_vim "$MNT"
  unbind_chroot
  FEATURES="$FEATURES,vim"
fi
if [[ $GO_INSTALL -eq 1 ]]; then
  install_go "$MNT" "$GO_VERSION"
  FEATURES="$FEATURES,go"
fi
if [[ $TASKFILE_INSTALL -eq 1 ]]; then
  install_taskfile "$MNT" "$TASKFILE_VERSION"
  FEATURES="$FEATURES,taskfile"
fi

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

# Bake the managed shell snippet (Go on PATH + operator aliases / appended bashrc
# files) once everything is installed and the run-as user's home exists. A no-op
# unless Go is baked or aliases/bashrc files were given.
if [[ $GO_INSTALL -eq 1 || ${#ALIASES[@]} -gt 0 || ${#BASHRC_FILES[@]} -gt 0 ]]; then
  install_shell_env "$MNT"
  FEATURES="$FEATURES,shellenv"
fi

# /etc/proteos-release — provenance the guest (and humans) can read.
BUILD_STAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)"
GO_REL="none"; [[ $GO_INSTALL -eq 1 ]] && GO_REL="$GO_VERSION"
TASK_REL="none"; [[ $TASKFILE_INSTALL -eq 1 ]] && TASK_REL="$TASKFILE_VERSION"
VIM_REL="no"; [[ $VIM_INSTALL -eq 1 ]] && VIM_REL="yes"
sudo tee "$MNT/etc/proteos-release" >/dev/null <<EOF
PROTEOS_ROOTFS_BASE=$BASE_NAME
PROTEOS_GUESTAGENT_VERSION=$VERSION
PROTEOS_GUESTAGENT_FEATURES=$FEATURES
PROTEOS_GIT_VERSION=${GIT_VERSION:-none}
PROTEOS_VIM=$VIM_REL
PROTEOS_GO_VERSION=$GO_REL
PROTEOS_TASKFILE_VERSION=$TASK_REL
PROTEOS_RUN_AS_USER=${USER_NAME:-root}
PROTEOS_CODESERVER_VERSION=${CS_VERSION:-none}
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
git_version    = ${GIT_VERSION:-none}
vim            = $VIM_REL
go_version     = $GO_REL
go_sha256      = ${GO_SHA256:-none}
taskfile_version = $TASK_REL
taskfile_sha256  = ${TASK_SHA256:-none}
codeserver_version = ${CS_VERSION:-none}
codeserver_sha256  = ${CS_SHA256:-none}
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
