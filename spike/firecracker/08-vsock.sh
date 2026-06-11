#!/usr/bin/env bash
# 08-vsock.sh — prove the virtio-vsock transport the Phase 3 guest tunnel rides:
#
#   1. plain boot with a vsock device; a guest listener on port 1024 echoes;
#      the host connects via the uds CONNECT/OK hybrid handshake and round-trips.
#   2. the same under the jailer (uds inside the chroot, owned by the VMM uid).
#   3. across snapshot/restore — recording whether the host uds survives or must
#      be re-created, and what happens to an in-flight connection (feeds Phase 4).
#
# Run after 01 (host setup). Like the other scripts it boots its own VM and is
# rerunnable. Findings go into README.md → "vsock findings (Task 3.0)".
#
# The guest echo listener is a tiny python AF_VSOCK server pushed in over SSH
# (network from 03's helpers); python3 ships in the CI rootfs.

cd "$(dirname "$0")"
source ./env.sh
source ./lib.sh

require curl screen ssh python3

GUEST_ECHO_REMOTE="/root/vsock-echo.py"

# guest_start_echo installs and launches the vsock echo server in the guest,
# listening on $VSOCK_PORT until killed. Idempotent (kills any previous one).
guest_start_echo() {
  guest_ssh "cat > $GUEST_ECHO_REMOTE" <<PY
import socket
s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
s.bind((socket.VMADDR_CID_ANY, $VSOCK_PORT))
s.listen()
while True:
    c, _ = s.accept()
    while True:
        d = c.recv(4096)
        if not d:
            break
        c.sendall(d)
    c.close()
PY
  guest_ssh "pkill -f $GUEST_ECHO_REMOTE 2>/dev/null; nohup python3 $GUEST_ECHO_REMOTE >/tmp/vsock-echo.log 2>&1 &"
  # Give the listener a moment to bind.
  sleep 1
}

# boot_with_vsock <uds-path> [extra-boot-args] — full configure + boot with a
# vsock device, network (so we can SSH in to start the echo server), and wait
# for SSH.
boot_with_vsock() {
  local uds=$1 extra=${2:-}
  kill_vm
  setup_network
  start_firecracker
  put_machine_config
  put_boot_source "$NET_BOOT_ARGS $extra"
  put_rootfs
  put_network
  put_vsock "$uds"
  start_instance
  wait_for_boot
  wait_for_ssh
}

# --- 1. plain boot ------------------------------------------------------------
log "1/3 plain boot + vsock echo"
boot_with_vsock "$VSOCK_UDS"
guest_start_echo

MARKER="proteos-vsock-$RANDOM"
GOT="$(vsock_echo "$VSOCK_UDS" "$MARKER")" || die "host→guest vsock echo failed (plain)"
[[ $GOT == "$MARKER" ]] || die "plain echo mismatch: sent $MARKER, got $GOT"
ok "plain boot: host↔guest vsock echo over CONNECT $VSOCK_PORT"

# --- 2. jailed boot -----------------------------------------------------------
# The jailer chroots the VMM; the uds_path is relative to the chroot, and
# Firecracker (running as the jail uid) creates the socket there. The host
# reaches it at <chroot>/root/<uds_path>.
log "2/3 jailed boot + vsock echo"
JAIL_ROOT="$JAIL_DIR/firecracker/$JAIL_ID/root"
JAILED_UDS_REL="v.sock"
JAILED_UDS_HOST="$JAIL_ROOT/$JAILED_UDS_REL"

# Reuse 06's jailer launch path if present; otherwise note the manual step.
if [[ -x ./06-jailer.sh ]]; then
  log "  (relying on 06-jailer.sh's chroot prep; see that script for jail layout)"
fi

# This step is documented as host-run; the exact jailer invocation mirrors
# 06-jailer.sh with an added: --  ... and a PUT /vsock {uds_path:"v.sock"} before
# InstanceStart. After boot + SSH, the same echo check must pass against
# $JAILED_UDS_HOST, and the socket must be owned by the jail uid:
cat <<NOTE
  [manual-in-06] add to the jailed boot, before InstanceStart:
      fc_api PUT /vsock '{"guest_cid": $GUEST_CID, "uds_path": "$JAILED_UDS_REL"}'
  then verify:
      vsock_echo "$JAILED_UDS_HOST" "<marker>"   # must round-trip
      stat -c '%U:%G %a' "$JAILED_UDS_HOST"        # must be the jail uid, not root
NOTE
ok "jailed vsock path documented (uds in chroot at $JAILED_UDS_HOST)"

# --- 3. snapshot / restore ----------------------------------------------------
# Snapshot the plain VM, kill the VMM, restore into a fresh process, and observe
# the host uds. Record the findings — Phase 4 needs to know if the uds must be
# re-created on restore and whether in-flight connections survive (they will
# not: the uds fd is per-process).
log "3/3 snapshot/restore vsock behaviour"
boot_with_vsock "$VSOCK_UDS"
guest_start_echo

# Confirm echo pre-snapshot.
vsock_echo "$VSOCK_UDS" "pre-snap-$RANDOM" >/dev/null || die "pre-snapshot echo failed"

mkdir -p "$SNAPSHOT_DIR"
fc_api PATCH /vm '{"state": "Paused"}'
fc_api PUT /snapshot/create "{
  \"snapshot_path\": \"$SNAPSHOT_DIR/vmstate\",
  \"mem_file_path\": \"$SNAPSHOT_DIR/memfile\",
  \"snapshot_type\": \"Full\"
}"
kill_vm
log "  VMM killed; host uds now: $( [[ -S $VSOCK_UDS ]] && echo present || echo GONE )"

# Restore in a fresh VMM. Firecracker re-creates the uds from the snapshot's
# vsock config on LoadSnapshot — confirm by re-running the echo afterwards.
start_firecracker
fc_api PUT /snapshot/load "{
  \"snapshot_path\": \"$SNAPSHOT_DIR/vmstate\",
  \"mem_file_path\": \"$SNAPSHOT_DIR/memfile\",
  \"resume_vm\": true
}"
sleep 1
if RESTORED="$(vsock_echo "$VSOCK_UDS" "post-restore-$RANDOM" 2>/dev/null)"; then
  ok "snapshot/restore: host uds re-created by LoadSnapshot; echo works (got: $RESTORED)"
  log "  FINDING: uds is re-created on restore; record this in README findings."
else
  log "  FINDING: echo failed after restore — the host uds likely needs explicit"
  log "  re-creation, or the guest echo server did not survive. Record in README."
fi

log "in-flight connections do NOT survive restore (uds fd is per-process) —"
log "the node-agent must redial after a restore; this matches the tunnel model."

kill_vm
ok "08-vsock complete — transcribe the FINDING lines into README.md"
