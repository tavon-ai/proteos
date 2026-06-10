#!/usr/bin/env bash
# 01 — Host setup: install the pinned firecracker + jailer binaries, fetch the
# pinned CI kernel, build the Ubuntu rootfs with a fresh SSH key, grant
# /dev/kvm access.
#
# Proves: this host (a Proxmox VM with nested KVM) can run Firecracker at all.
# Records: the exact artifact versions in versions.lock (commit that file).

cd "$(dirname "${BASH_SOURCE[0]}")"
source ./env.sh
source ./lib.sh

require curl wget tar unsquashfs ssh-keygen screen setfacl truncate awk
[[ $ARCH == x86_64 ]] || die "spike assumes x86_64, got $ARCH — see 00-proxmox-vm.md"
[[ -e /dev/kvm ]] || die "/dev/kvm missing — nested virt is not enabled; see 00-proxmox-vm.md"

mkdir -p "$BIN_DIR" "$IMG_DIR" "$RUN_DIR"

# --- 1. firecracker + jailer (pinned release) ---------------------------------
if [[ ! -x $BIN_DIR/firecracker ]]; then
  log "downloading firecracker $FC_VERSION"
  curl -fsSL "$FC_RELEASE_URL/download/$FC_VERSION/firecracker-$FC_VERSION-$ARCH.tgz" |
    tar -xz -C "$WORK_DIR"
  install -m 755 "$WORK_DIR/release-$FC_VERSION-$ARCH/firecracker-$FC_VERSION-$ARCH" "$BIN_DIR/firecracker"
  install -m 755 "$WORK_DIR/release-$FC_VERSION-$ARCH/jailer-$FC_VERSION-$ARCH" "$BIN_DIR/jailer"
  rm -rf "$WORK_DIR/release-$FC_VERSION-$ARCH"
fi
ok "$("$BIN_DIR/firecracker" --version | head -n1)"

# --- 2. guest kernel (newest vmlinux under the *pinned* CI line) ---------------
if [[ ! -f $KERNEL ]]; then
  log "resolving newest vmlinux under firecracker-ci/$CI_VERSION"
  # Keep curl (network) and grep (no-match) failures separate: under
  # `set -euo pipefail` a no-match grep inside a command substitution would
  # abort the script before the die below could explain why.
  listing=$(curl -fsS "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/$CI_VERSION/$ARCH/vmlinux-&list-type=2") ||
    die "could not list the CI bucket for firecracker-ci/$CI_VERSION/$ARCH"
  kernel_key=$(grep -oP "(?<=<Key>)(firecracker-ci/$CI_VERSION/$ARCH/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3})(?=</Key>)" <<<"$listing" |
    sort -V | tail -1) || true
  [[ -n $kernel_key ]] || die "no vmlinux under firecracker-ci/$CI_VERSION/$ARCH — does that CI line exist? The CI bucket lags the $FC_VERSION binary; lower CI_VERSION in env.sh."
  log "downloading $kernel_key"
  wget -q -O "$KERNEL" "$CI_BUCKET/$kernel_key"
  echo "$kernel_key" >"$IMG_DIR/kernel.key"
fi
ok "kernel: $(cat "$IMG_DIR/kernel.key")"

# --- 3. rootfs: CI squashfs → ext4 with our own SSH key ------------------------
if [[ ! -f $ROOTFS ]]; then
  log "downloading ubuntu-$UBUNTU_VERSION squashfs"
  wget -q -O "$IMG_DIR/ubuntu.squashfs" \
    "$CI_BUCKET/firecracker-ci/$CI_VERSION/$ARCH/ubuntu-$UBUNTU_VERSION.squashfs"

  log "building ext4 rootfs with a fresh SSH key"
  rm -rf "$IMG_DIR/squashfs-root"
  # Non-root unsquashfs warns about device nodes it can't create; harmless —
  # the guest mounts devtmpfs.
  unsquashfs -d "$IMG_DIR/squashfs-root" "$IMG_DIR/ubuntu.squashfs" >/dev/null
  ssh-keygen -q -f "$SSH_KEY" -N ""
  mkdir -p "$IMG_DIR/squashfs-root/root/.ssh"
  cp "$SSH_KEY.pub" "$IMG_DIR/squashfs-root/root/.ssh/authorized_keys"
  sudo chown -R root:root "$IMG_DIR/squashfs-root"
  truncate -s 1G "$ROOTFS"
  sudo mkfs.ext4 -q -d "$IMG_DIR/squashfs-root" -F "$ROOTFS"
  sudo rm -rf "$IMG_DIR/squashfs-root" "$IMG_DIR/ubuntu.squashfs"
fi
ok "rootfs: $ROOTFS"

# --- 4. /dev/kvm access for the current user -----------------------------------
sudo setfacl -m "u:${USER}:rw" /dev/kvm
[[ -r /dev/kvm && -w /dev/kvm ]] || die "/dev/kvm not read/writable after setfacl"
ok "/dev/kvm accessible as $USER"

# --- 5. record exactly what this run used (commit this file) --------------------
cat >versions.lock <<EOF
# Written by 01-host-setup.sh — exact artifacts this spike ran against.
firecracker=$FC_VERSION
kernel=$(cat "$IMG_DIR/kernel.key")
rootfs=firecracker-ci/$CI_VERSION/$ARCH/ubuntu-$UBUNTU_VERSION.squashfs
host=$(. /etc/os-release && echo "$PRETTY_NAME") $(uname -r)
date=$(date -u +%FT%TZ)
EOF
ok "wrote versions.lock — commit it with your findings"
log "next: ./02-boot-vm.sh"
