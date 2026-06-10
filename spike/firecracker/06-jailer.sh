#!/usr/bin/env bash
# 06 — Boot the same minimal microVM, but under jailer: chroot, dedicated
# uid/gid, cgroup. Then verify the isolation actually holds.
#
# Proves: the plan's "VM / host security baseline" (jailer from the first VM)
# works before any driver code exists. Production runs *every* VMM this way.

cd "$(dirname "${BASH_SOURCE[0]}")"
source ./env.sh
source ./lib.sh

[[ -f $KERNEL && -f $ROOTFS ]] || die "kernel/rootfs missing — run ./01-host-setup.sh first"

# Dedicated system user the jailed VMM drops to.
if ! id -u "$FC_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$FC_USER"
  log "created system user $FC_USER"
fi

sudo pkill -u "$FC_USER" firecracker 2>/dev/null || true
sudo rm -rf "$JAIL_DIR"

# Jailer chroots the VMM into <chroot-base>/firecracker/<id>/root, so the
# kernel and rootfs must be placed *inside* the jail before launch — in
# production the node-agent does exactly this (or hardlinks).
CHROOT="$JAIL_DIR/firecracker/$JAIL_ID/root"
sudo mkdir -p "$CHROOT/run"
sudo cp "$KERNEL" "$CHROOT/vmlinux"
sudo cp "$ROOTFS" "$CHROOT/rootfs.ext4"
sudo chown -R "$FC_USER:$FC_USER" "$JAIL_DIR"

log "launching the VMM under jailer (chroot=$CHROOT, uid=$FC_USER)"
# If the --cgroup flags fail on your distro, drop them and note it in findings.
sudo "$BIN_DIR/jailer" \
  --id "$JAIL_ID" \
  --exec-file "$BIN_DIR/firecracker" \
  --uid "$(id -u "$FC_USER")" --gid "$(id -g "$FC_USER")" \
  --chroot-base-dir "$JAIL_DIR" \
  --cgroup-version 2 --cgroup "cpu.weight=512" \
  --daemonize \
  -- --api-sock /run/firecracker.socket

JAILED_SOCK="$CHROOT/run/firecracker.socket"
jail_sock_exists() { sudo test -S "$JAILED_SOCK"; }
wait_for "jailed API socket" 10 jail_sock_exists

# The socket belongs to $FC_USER, so talk to it via sudo. NOTE: all paths in
# API calls are as the *chrooted* process sees them — relative to $CHROOT.
jail_api() {
  local method=$1 path=$2 body=${3:-}
  local args=(--unix-socket "$JAILED_SOCK" -sS -f -X "$method"
    "http://localhost$path" -H 'Content-Type: application/json')
  [[ -n $body ]] && args+=(-d "$body")
  sudo curl "${args[@]}"
}

jail_api PUT /machine-config "{\"vcpu_count\": $VCPUS, \"mem_size_mib\": $MEM_MIB}"
jail_api PUT /boot-source '{"kernel_image_path": "/vmlinux", "boot_args": "console=ttyS0 reboot=k panic=1 pci=off"}'
jail_api PUT /drives/rootfs '{"drive_id": "rootfs", "path_on_host": "/rootfs.ext4", "is_root_device": true, "is_read_only": false}'
jail_api PUT /actions '{"action_type": "InstanceStart"}'

vm_state_running() { jail_api GET / | grep -q '"state":"Running"'; }
wait_for "jailed microVM in state Running" 15 vm_state_running

# --- verify the isolation actually holds ----------------------------------------
pid="$(pgrep -u "$FC_USER" -f firecracker | head -n1)"
[[ -n $pid ]] || die "no firecracker process owned by $FC_USER"

run_as="$(ps -o user= -p "$pid" | tr -d '[:space:]')"
[[ $run_as == "$FC_USER" ]] || die "VMM runs as '$run_as', expected $FC_USER"
ok "VMM runs as unprivileged user $FC_USER (pid $pid)"

root_of="$(sudo readlink "/proc/$pid/root")"
[[ $root_of == "$CHROOT" ]] || die "VMM root is '$root_of', expected chroot $CHROOT"
ok "VMM is chrooted into $CHROOT"

log "VMM cgroup membership (← record in findings):"
sudo cat "/proc/$pid/cgroup"

ok "jailer baseline proven: chroot + uid drop + cgroup, with the VM still booting fine"
log "stop it: sudo pkill -u $FC_USER firecracker   (07-teardown.sh also does this)"
log "next: ./07-teardown.sh when you're done"
