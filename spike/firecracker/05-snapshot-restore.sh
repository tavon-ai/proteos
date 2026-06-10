#!/usr/bin/env bash
# 05 — Pause the VM, take a full snapshot (memory + device state), KILL the
# VMM, restore the snapshot into a brand-new VMM process, and verify a
# background process survived. Records the two findings the plan cares about:
# clock skew and RNG/entropy state after restore (these feed Phase 4's resume
# acceptance criteria).
#
# Proves: hibernate (stop = snapshot) and resume (start = restore) — the core
# of the machine lifecycle.

cd "$(dirname "${BASH_SOURCE[0]}")"
source ./env.sh
source ./lib.sh

[[ -f $KERNEL && -f $ROOTFS ]] || die "kernel/rootfs missing — run ./01-host-setup.sh first"

kill_vm
setup_network

log "booting a VM with networking"
start_firecracker
put_machine_config
put_boot_source "$NET_BOOT_ARGS"
put_rootfs
put_network
start_instance
wait_for_ssh

log "starting a heartbeat process in the guest (1 line/sec)"
guest_ssh "nohup sh -c 'while true; do date >> /tmp/heartbeat; sleep 1; done' >/dev/null 2>&1 & sleep 0.2"
sleep 2

log "pausing and snapshotting"
mkdir -p "$SNAPSHOT_DIR"
fc_api PATCH /vm '{"state": "Paused"}'
snap_start=$(date +%s%3N)
fc_api PUT /snapshot/create "{
  \"snapshot_type\": \"Full\",
  \"snapshot_path\": \"$SNAPSHOT_DIR/vmstate\",
  \"mem_file_path\": \"$SNAPSHOT_DIR/mem\"
}"
ok "snapshot written in $(($(date +%s%3N) - snap_start))ms ← record in findings"
ls -lh "$SNAPSHOT_DIR" # note: mem file ≈ guest RAM size — this is the storage cost of hibernate

log "killing the VMM — the original process is gone for good"
kill_vm

log "waiting 20s so clock skew after restore is clearly observable"
sleep 20

log "restoring into a fresh VMM process (tap device is still in place)"
restore_start=$(date +%s%3N)
start_firecracker
fc_api PUT /snapshot/load "{
  \"snapshot_path\": \"$SNAPSHOT_DIR/vmstate\",
  \"mem_backend\": {\"backend_type\": \"File\", \"backend_path\": \"$SNAPSHOT_DIR/mem\"},
  \"resume_vm\": true
}"
ok "restored + resumed in $(($(date +%s%3N) - restore_start))ms ← record in findings"

# Any TCP/TLS connection open at snapshot time is dead now — this SSH is fresh.
wait_for_ssh

h1="$(guest_ssh 'wc -l < /tmp/heartbeat' | tr -d '[:space:]')"
sleep 3
h2="$(guest_ssh 'wc -l < /tmp/heartbeat' | tr -d '[:space:]')"
[[ $h2 -gt $h1 ]] || die "heartbeat is not advancing — background process did not survive restore"
ok "background process survived snapshot/restore ($h1 → $h2 heartbeats)"

# --- the findings the plan explicitly asks for ---------------------------------
guest_now="$(guest_ssh 'date +%s')"
host_now="$(date +%s)"
log "CLOCK SKEW after restore: $((host_now - guest_now))s (guest is behind by ~the time spent hibernated)"
log "  ← record this; it's why the Phase 4 resume path must resync the guest clock"

log "RNG state in the restored guest (← record; feeds the entropy-reseed requirement):"
guest_ssh "dmesg | grep -i -E 'random|rng' | tail -n5; echo '---'; cat /proc/sys/kernel/random/entropy_avail" || true
log "  note: kernels ≥5.18 pin entropy_avail at 256 — the real question is whether"
log "  the CRNG was reseeded after restore (look for 'crng reseeded' in dmesg above)"

ok "snapshot/restore proven end-to-end"
log "next: ./06-jailer.sh"
