#!/usr/bin/env bash
# 02 — Boot a minimal microVM (no network) via the Firecracker API socket and
# verify the kernel reaches a login prompt on the serial console.
#
# Proves: the API-driven configure-then-boot flow the node-agent driver will
# use (machine-config, boot-source, rootfs drive, InstanceStart).

cd "$(dirname "${BASH_SOURCE[0]}")"
source ./env.sh
source ./lib.sh

[[ -f $KERNEL && -f $ROOTFS ]] || die "kernel/rootfs missing — run ./01-host-setup.sh first"

kill_vm # clean slate; safe if nothing is running
start_firecracker

log "configuring the microVM over the API socket"
put_machine_config
put_boot_source
put_rootfs

log "booting"
boot_start=$(date +%s%3N)
start_instance
wait_for_boot
ok "boot reached login prompt in $(($(date +%s%3N) - boot_start))ms ← record in findings"

log "serial console: screen -r $SCREEN_SESSION   (detach: Ctrl-A d)"
log "console log:    $VM_LOG"
log "the VM is left running; ./03-network.sh replaces it, ./07-teardown.sh removes it"
