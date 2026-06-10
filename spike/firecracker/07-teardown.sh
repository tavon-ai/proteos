#!/usr/bin/env bash
# 07 — Remove everything the spike created on the host. Safe to run
# repeatedly; safe to run between scripts to get back to a clean slate.
#
#   ./07-teardown.sh         kill VMs, remove tap/NAT rules, runtime state, jail
#   ./07-teardown.sh --all   also delete downloaded binaries/kernel/rootfs/disks
#
# Deliberately left in place (remove by hand if you're done with the VM):
#   - the /dev/kvm ACL:    sudo setfacl -x "u:$USER" /dev/kvm
#   - the fc-spike user:   sudo userdel fc-spike

cd "$(dirname "${BASH_SOURCE[0]}")"
source ./env.sh
source ./lib.sh

log "stopping VMs"
kill_vm
sudo pkill -u "$FC_USER" firecracker 2>/dev/null || true

log "removing tap device and NAT rules"
teardown_network

log "removing runtime state and jail"
rm -rf "$RUN_DIR"
sudo rm -rf "$JAIL_DIR"

if [[ ${1:-} == "--all" ]]; then
  log "removing all downloaded artifacts ($WORK_DIR)"
  rm -rf "$WORK_DIR"
fi

ok "teardown complete$( [[ ${1:-} == "--all" ]] && echo ' (including artifacts)')"
