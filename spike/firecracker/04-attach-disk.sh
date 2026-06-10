#!/usr/bin/env bash
# 04 — Attach a second block device (the future "persistent disk"), write a
# file to it, stop the VM completely, cold-boot a fresh VM with the same disk,
# and verify the file survived.
#
# Proves: the disk attach/persist model behind Phase 4 (host-local disk image
# attached as a drive; data outlives the VMM process).

cd "$(dirname "${BASH_SOURCE[0]}")"
source ./env.sh
source ./lib.sh

[[ -f $KERNEL && -f $ROOTFS ]] || die "kernel/rootfs missing — run ./01-host-setup.sh first"

if [[ ! -f $DATA_DISK ]]; then
  log "creating 256M ext4 data disk image"
  truncate -s 256M "$DATA_DISK"
  mkfs.ext4 -q -F "$DATA_DISK"
fi

boot_with_data_disk() {
  start_firecracker
  put_machine_config
  put_boot_source "$NET_BOOT_ARGS"
  put_rootfs
  put_data_disk # appears in the guest as /dev/vdb
  put_network
  start_instance
  wait_for_ssh
}

kill_vm
setup_network

log "first boot: write proof file to the data disk"
boot_with_data_disk
stamp="persistence-proof $(date -u +%FT%TZ) $$"
guest_ssh "mkdir -p /mnt && mount /dev/vdb /mnt && echo '$stamp' > /mnt/proof.txt && sync && umount /mnt"
ok "wrote: $stamp"

log "stopping the VM completely (guest 'reboot' exits the VMM under reboot=k)"
guest_ssh "reboot" 2>/dev/null || true
wait_for "VMM process exit" 15 vm_exited

log "cold boot: fresh VMM, same data disk"
boot_with_data_disk
found="$(guest_ssh "mkdir -p /mnt && mount /dev/vdb /mnt && cat /mnt/proof.txt")"
[[ $found == "$stamp" ]] || die "proof file mismatch: wrote '$stamp', read '$found'"
ok "file survived a full stop + cold boot — disk persistence proven"
log "next: ./05-snapshot-restore.sh"
