#!/usr/bin/env bash
# 09 — Encrypted machine-volume + hibernate/resume: the Phase 4 risk burn-down.
#
# Stitches 04 (disk persist) + 05 (snapshot/restore) + 06 (jailer) + 08 (vsock)
# into the exact shape the FirecrackerDriver uses (plan Phase 4, decision #1):
#
#   - ONE LUKS2 container file per machine, OUTSIDE the jail tree, opened to
#     /dev/mapper/proteos-<id8>, ext4, mounted at <chroot>/state. Because the
#     mount exists before the jailer launches, the chrooted VMM inherits it and
#     sees it as /state (the central "same mount ns" claim — verified here).
#   - That one encrypted mount holds ALL mutable per-machine state:
#       /state/rootfs.ext4   writable rootfs copy (preserved across hibernate)
#       /state/data.ext4     persistent disk → guest /dev/vdb
#       /state/snap/{vmstate,mem}   snapshot written by Firecracker onto the mount
#   - stop = hibernate: pause → snapshot to /state/snap → kill VMM → umount →
#     luksClose. The raw container file is then opaque (no-plaintext grep proof).
#   - start = resume: luksOpen → rebuild jail scaffolding (never touches the
#     external volume) → mount → fresh jailed VMM → LoadSnapshot(resume_vm) →
#     guest /resume hygiene is a node-agent job (not exercised here; see 4.3).
#
# Proves: the file survives, a PRE-HIBERNATE BACKGROUND PROCESS is still running
# after resume (the RAM came back), the snapshot lived only inside the encrypted
# volume, and records the Phase-4 findings (VMGenID/CRNG reseed, clock skew,
# snapshot/restore timings, mem-file size). vsock-under-jailer echo is recorded
# as a bonus (closes 08's open jailed-vsock item) but is non-fatal so a vsock
# hiccup never masks the encrypted hibernate/resume result.
#
# Run after 01 (host setup). Rerunnable: starts by tearing down any previous VM,
# mount, and mapper. Transcribe the FINDING lines into README.md.

cd "$(dirname "${BASH_SOURCE[0]}")"
source ./env.sh
source ./lib.sh

require curl screen ssh python3 cryptsetup mkfs.ext4 blkid

[[ -f $KERNEL && -f $ROOTFS ]] || die "kernel/rootfs missing — run ./01-host-setup.sh first"

# --- 09-local config (kept out of env.sh; this is throwaway spike state) -------
MID="0009spike-encrypted-machine-volume"   # stands in for a real machine UUID
MAPPER_NAME="proteos-${MID:0:8}"            # /dev/mapper/proteos-<id8> (decision #1)
MAPPER="/dev/mapper/$MAPPER_NAME"
VOLUMES_DIR="$WORK_DIR/volumes"             # OUTSIDE the jail tree, per decision #1
VOLUME_FILE="$VOLUMES_DIR/${MID}.luks"
KEYFILE="$RUN_DIR/${MID}.key"               # 32B volume key. In prod the control
                                            # plane mints + holds this and ships it
                                            # in EnsureRequest; the host never keeps
                                            # it next to the ciphertext (decision #2).
DISK_MIB=256                                # the guest /dev/vdb persistent disk

# Jail layout (mirrors 06). The same id is reused for cold + resume because each
# boot rebuilds the scaffolding from scratch — so there is never a stale socket,
# stale v.sock, or leftover cgroup to trip over.
JAIL_ID="enc"
CHROOT="$JAIL_DIR/firecracker/$JAIL_ID/root"
JAILED_SOCK="$CHROOT/run/firecracker.socket"
JAILED_VSOCK_REL="v.sock"                   # uds_path is relative to the chroot
JAILED_VSOCK_HOST="$CHROOT/$JAILED_VSOCK_REL"

GUEST_ECHO_REMOTE="/root/vsock-echo.py"
MARKER="proteos-enc-$RANDOM-$$"             # written to /dev/vdb; must survive resume
                                            #   and must NOT appear in the raw .luks

# Encrypted-path findings → committable artifacts (see lib.sh finding_*). Kept
# separate from 10's plain-path findings.{json,md}.
FINDINGS_TSV="$RUN_DIR/encrypted-findings.tsv"
ENC_ARTIFACT_DIR="${ARTIFACT_DIR:-$(pwd)}"
finding_reset

# --- jailed API helper (sudo: the socket is owned by $FC_USER) -----------------
# Paths in every request are as the *chrooted* VMM sees them — relative to $CHROOT.
# Mirrors lib.sh fc_api: prints Firecracker's fault_message on non-2xx and returns
# non-zero so callers under `set -e` abort with the real reason.
japi() {
  local method=$1 path=$2 reqbody=${3:-}
  local args=(--unix-socket "$JAILED_SOCK" -sS -X "$method"
    "http://localhost$path" -H 'Content-Type: application/json' -w '\n%{http_code}')
  [[ -n $reqbody ]] && args+=(-d "$reqbody")
  local out status respbody
  out=$(sudo curl "${args[@]}") || return 1
  status=${out##*$'\n'}
  respbody=${out%$'\n'*}
  if ((status < 200 || status >= 300)); then
    printf '\e[1;31m[fail]\e[0m %s %s → HTTP %s: %s\n' "$method" "$path" "$status" "$respbody" >&2
    return 1
  fi
  [[ -n $respbody ]] && printf '%s' "$respbody"
  return 0
}

jail_sock_exists() { sudo test -S "$JAILED_SOCK"; }
jail_state_running() { japi GET / 2>/dev/null | grep -q '"state":"Running"'; }

# --- teardown / open / close --------------------------------------------------
unmount_state() { mountpoint -q "$CHROOT/state" 2>/dev/null && sudo umount "$CHROOT/state" || true; }
luks_close() { [[ -e $MAPPER ]] && sudo cryptsetup close "$MAPPER_NAME" || true; }

kill_jailed_vm() {
  sudo pkill -u "$FC_USER" firecracker 2>/dev/null || true
  wait_for "jailed VMM exit" 15 bash -c "! sudo test -S '$JAILED_SOCK'" 2>/dev/null || true
}

cleanup_prev() {
  kill_jailed_vm
  unmount_state
  luks_close
  sudo rm -rf "$JAIL_DIR"
}

luks_open() {
  sudo cryptsetup open --type luks2 --key-file "$KEYFILE" "$VOLUME_FILE" "$MAPPER_NAME"
}

# Build a fresh jail chroot and mount the (already-open) encrypted volume into it
# BEFORE the jailer launches, so /state is inherited into the VMM's mount ns.
# rm -rf is safe: the external volume is unmounted at this point, so nothing on
# the encrypted mount is touched — this is the spike's "scaffolding refresh never
# touches /state" (prepareResumeJail) in miniature.
prepare_jail_and_mount() {
  sudo rm -rf "$JAIL_DIR/firecracker/$JAIL_ID"
  sudo mkdir -p "$CHROOT/run" "$CHROOT/state"
  sudo cp "$KERNEL" "$CHROOT/vmlinux"
  sudo chown -R "$FC_USER:$FC_USER" "$JAIL_DIR"

  sudo mount "$MAPPER" "$CHROOT/state"
  sudo chown -R "$FC_USER:$FC_USER" "$CHROOT/state"   # VMM (as $FC_USER) opens the
                                                      # drives + writes the snapshot
}

# No --cgroup here: cgroup placement is 06's concern, and dropping it keeps the
# same-id relaunch on resume free of "cgroup exists" snags. --netns is omitted so
# the host tap from setup_network is reachable directly (06 booted the same way).
launch_jailer() {
  require_kvm
  sudo "$BIN_DIR/jailer" \
    --id "$JAIL_ID" \
    --exec-file "$BIN_DIR/firecracker" \
    --uid "$(id -u "$FC_USER")" --gid "$(id -g "$FC_USER")" \
    --chroot-base-dir "$JAIL_DIR" \
    --daemonize \
    -- --api-sock /run/firecracker.socket
  wait_for "jailed API socket" 10 jail_sock_exists
}

# Pre-boot device + NIC + vsock config shared by cold boot (drives load from a
# fresh rootfs) — resume replaces all of this with a single LoadSnapshot.
configure_devices() {
  japi PUT /machine-config "{\"vcpu_count\": $VCPUS, \"mem_size_mib\": $MEM_MIB}"
  japi PUT /boot-source "{\"kernel_image_path\": \"/vmlinux\",
    \"boot_args\": \"console=ttyS0 reboot=k panic=1 pci=off $NET_BOOT_ARGS\"}"
  japi PUT /drives/rootfs '{"drive_id":"rootfs","path_on_host":"/state/rootfs.ext4","is_root_device":true,"is_read_only":false}'
  japi PUT /drives/data   '{"drive_id":"data","path_on_host":"/state/data.ext4","is_root_device":false,"is_read_only":false}'
  japi PUT /network-interfaces/eth0 "{\"iface_id\":\"eth0\",\"guest_mac\":\"$GUEST_MAC\",\"host_dev_name\":\"$TAP_DEV\"}"
  japi PUT /vsock "{\"guest_cid\": $GUEST_CID, \"uds_path\": \"$JAILED_VSOCK_REL\"}"
}

# Tiny AF_VSOCK echo server (trimmed from 08): binds synchronously then
# double-forks, so the launching ssh returns 0 only once the listener is up.
guest_start_echo() {
  guest_ssh "cat > $GUEST_ECHO_REMOTE" <<PY || return 1
import socket, os, sys
try:
    s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
    s.bind((socket.VMADDR_CID_ANY, $VSOCK_PORT)); s.listen()
except Exception as e:
    open("/tmp/vsock-echo.log","w").write("vsock-echo: %s\n" % e); sys.exit(1)
if os.fork(): os._exit(0)
os.setsid()
if os.fork(): os._exit(0)
n=os.open("/dev/null", os.O_RDWR); os.dup2(n,0); os.dup2(n,1); os.dup2(n,2)
while True:
    c,_=s.accept()
    while True:
        d=c.recv(4096)
        if not d: break
        c.sendall(d)
    c.close()
PY
  guest_ssh "pkill -f $GUEST_ECHO_REMOTE 2>/dev/null; rm -f /tmp/vsock-echo.log; true" || true
  guest_ssh "python3 $GUEST_ECHO_REMOTE"
}

# ==============================================================================
# 0. clean slate + dedicated jail user (06)
# ==============================================================================
cleanup_prev
setup_network   # tap-spike0 persists across the hibernate window → "same tap name"

if ! id -u "$FC_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$FC_USER"
  log "created system user $FC_USER"
fi

# ==============================================================================
# 1. provision the encrypted machine volume (decision #1 sizing)
# ==============================================================================
mkdir -p "$VOLUMES_DIR" "$RUN_DIR"
rootfs_mib=$(( ($(stat -c%s "$ROOTFS") + 1048575) / 1048576 ))
VOL_MIB=$(( rootfs_mib + MEM_MIB + DISK_MIB + 512 ))   # rootfs + RAM + disk + slack
log "minting 32B volume key + provisioning ${VOL_MIB} MiB LUKS2 container"
head -c 32 /dev/urandom > "$KEYFILE"; chmod 600 "$KEYFILE"

# Rerunnable: keep an existing container, otherwise format a new one.
if [[ ! -f $VOLUME_FILE ]]; then
  truncate -s "${VOL_MIB}M" "$VOLUME_FILE"
  sudo cryptsetup luksFormat --type luks2 --batch-mode --key-file "$KEYFILE" "$VOLUME_FILE"
  ok "LUKS2 container formatted ($VOLUME_FILE)"
else
  log "reusing existing container $VOLUME_FILE (key must match a prior run)"
fi
luks_open
sudo blkid "$MAPPER" >/dev/null 2>&1 || sudo mkfs.ext4 -q -F "$MAPPER"
ok "opened $MAPPER (ext4)"

# Mount into a fresh chroot, then (re)create the per-machine files on the mount.
prepare_jail_and_mount
log "seeding /state: fresh rootfs copy + ${DISK_MIB}M data disk + snap dir"
sudo cp "$ROOTFS" "$CHROOT/state/rootfs.ext4"
sudo truncate -s "${DISK_MIB}M" "$CHROOT/state/data.ext4"
sudo mkfs.ext4 -q -F "$CHROOT/state/data.ext4"
sudo mkdir -p "$CHROOT/state/snap"
sudo chown -R "$FC_USER:$FC_USER" "$CHROOT/state"

# ==============================================================================
# 2. cold boot (jailed) — proves the chrooted VMM sees /state
# ==============================================================================
log "cold boot under jailer (drives load from /state → /state is visible inside)"
launch_jailer
configure_devices
sudo chmod 666 "$JAILED_VSOCK_HOST" 2>/dev/null || true   # let $USER reach the FC_USER-owned uds
japi PUT /actions '{"action_type": "InstanceStart"}'
wait_for "jailed microVM Running" 15 jail_state_running
ok "VMM booted with rootfs+data on the encrypted /state mount"
wait_for_ssh

log "writing proof file to the persistent disk (/dev/vdb) + starting a heartbeat"
guest_ssh "mkdir -p /persist && mount /dev/vdb /persist && echo '$MARKER' > /persist/proof.txt && sync"
guest_ssh "nohup sh -c 'while true; do date >> /persist/heartbeat; sleep 1; done' >/dev/null 2>&1 & sleep 0.2"
sleep 2
ok "wrote /persist/proof.txt = '$MARKER' and a 1Hz heartbeat (both on /dev/vdb)"

# Bonus: vsock-under-jailer echo (closes 08's open jailed item). Non-fatal.
if guest_start_echo && \
   GOT="$(vsock_echo "$JAILED_VSOCK_HOST" "ping-$RANDOM" 2>/dev/null)" && [[ -n $GOT ]]; then
  ok "jailed vsock echo round-trips (uds at $JAILED_VSOCK_HOST, got: $GOT)"
  log "  FINDING(09-vsock): jailed host↔guest vsock works pre-hibernate."
else
  log "  FINDING(09-vsock): jailed vsock echo did NOT round-trip — record perms"
  log "  ($(stat -c '%U:%G %a' "$JAILED_VSOCK_HOST" 2>/dev/null || echo 'no uds')) and revisit; non-fatal here."
fi

# ==============================================================================
# 3. hibernate = pause → snapshot to /state/snap → kill → umount → luksClose
# ==============================================================================
log "hibernating: pause + Full snapshot onto the encrypted mount"
guest_dmesg_baseline   # clear the ring buffer so a post-resume vmgenid reseed is unambiguous
japi PATCH /vm '{"state": "Paused"}'
snap_start=$(date +%s%3N)
japi PUT /snapshot/create '{"snapshot_type":"Full",
  "snapshot_path":"/state/snap/vmstate","mem_file_path":"/state/snap/mem"}'
snap_ms=$(( $(date +%s%3N) - snap_start ))
mem_bytes=$(sudo stat -c%s "$CHROOT/state/snap/mem")
ok "snapshot written in ${snap_ms}ms; mem file = $(( mem_bytes / 1048576 )) MiB ← record in findings"
finding_set "Encrypted snapshot create time (09)" "${snap_ms} ms" "Full snapshot written onto the LUKS mount"
finding_set "Encrypted snapshot mem size (09)" "$(( mem_bytes / 1048576 )) MiB" "lives inside the volume only (decision #1)"
log "  snapshot files live ONLY inside the encrypted volume:"
sudo ls -lh "$CHROOT/state/snap"

kill_jailed_vm
unmount_state
luks_close
ok "VMM killed, /state unmounted, volume closed — machine is fully at rest"

# no-plaintext-at-rest proof (cheap subset of 4.6's grep): the marker we wrote in
# the guest must NOT be readable in the raw container now that it is luksClose'd.
if sudo grep -a -q -- "$MARKER" "$VOLUME_FILE"; then
  die "PLAINTEXT LEAK: marker '$MARKER' found in raw $VOLUME_FILE while closed"
fi
ok "no-plaintext-at-rest: marker absent from the closed container (encrypted at rest)"
finding_set "No plaintext at rest (09)" "pass" "guest marker not greppable in the closed LUKS container"

log "sleeping 20s so post-resume clock skew is clearly observable (05 did the same)"
sleep 20

# ==============================================================================
# 4. resume = luksOpen → rebuild jail → mount → fresh VMM → LoadSnapshot
# ==============================================================================
log "resuming: reopen volume, fresh jailed VMM, LoadSnapshot(resume_vm)"
luks_open
prepare_jail_and_mount            # fresh chroot ⇒ no stale v.sock/socket to rm (08's
                                  #   rm-stale-uds is satisfied by rebuilding the jail)
launch_jailer
restore_start=$(date +%s%3N)
japi PUT /snapshot/load '{"snapshot_path":"/state/snap/vmstate",
  "mem_backend":{"backend_type":"File","backend_path":"/state/snap/mem"},
  "resume_vm":true}'
restore_ms=$(( $(date +%s%3N) - restore_start ))
ok "restored + resumed in ${restore_ms}ms ← record in findings"
finding_set "Encrypted restore + resume time (09)" "${restore_ms} ms" "luksOpen+mount+LoadSnapshot(resume_vm) from the volume"

# Any pre-snapshot TCP/TLS is dead; this SSH is fresh (the gateway redials too).
wait_for_ssh

# --- the assertions: file survived AND the RAM/process came back --------------
found="$(guest_ssh 'cat /persist/proof.txt' | tr -d '\r')"
[[ $found == "$MARKER" ]] || die "proof file mismatch after resume: wrote '$MARKER', read '$found'"
ok "file survived encrypted hibernate/resume: /persist/proof.txt = '$found'"

h1="$(guest_ssh 'wc -l < /persist/heartbeat' | tr -d '[:space:]')"
sleep 3
h2="$(guest_ssh 'wc -l < /persist/heartbeat' | tr -d '[:space:]')"
[[ $h2 -gt $h1 ]] || die "heartbeat not advancing — the pre-hibernate process did NOT survive"
ok "PRE-HIBERNATE background process is still running after resume ($h1 → $h2 lines)"

# --- the Phase-4 findings the plan asks 09 to record --------------------------
guest_now="$(guest_ssh 'date +%s')"; host_now="$(date +%s)"
finding_set "Encrypted clock skew after resume (09)" "$(( host_now - guest_now )) s" \
  "≈ the ~20s hibernated; node-agent PUT /resume must resync (decision #9)"
log "FINDING(clock): guest is $((host_now - guest_now))s behind the host after resume"
log "  ← ≈ the hibernated duration; the node-agent PUT /resume must resync the clock (decision #9)"

vmgen="$(guest_vmgenid_probe)"
finding_set "Encrypted CRNG reseeded after resume? (09)" "${vmgen%%$'\t'*}" "${vmgen#*$'\t'}"
log "FINDING(vmgenid/CRNG): did the restore reseed the guest CRNG? (look for crng/vmgenid):"
guest_ssh "dmesg | grep -i -E 'crng|vmgenid|random' | tail -n8; echo '---entropy_avail---'; cat /proc/sys/kernel/random/entropy_avail" || true
log "  ← if 'crng reseeded' / vmgenid appears, entropy injection is belt-and-braces;"
log "    if not, the PUT /resume RNDADDENTROPY step is load-bearing (decision #9)."

# Consume the snapshot (stale RAM must never be restored twice — matches decision #4).
sudo rm -f "$CHROOT/state/snap/"* || true
ok "snapshot consumed (/state/snap cleared)"

finding_finalize "$ENC_ARTIFACT_DIR/encrypted-findings.json" "$ENC_ARTIFACT_DIR/encrypted-findings.md" \
  "Firecracker spike findings — encrypted path (09)"

log "leaving the resumed VM running. Stop it: sudo pkill -u $FC_USER firecracker"
log "full teardown: ./07-teardown.sh   (then cleanup vol: sudo cryptsetup close $MAPPER_NAME; rm -rf $VOLUMES_DIR)"
ok "09 complete — encrypted hibernate/resume proven; transcribe FINDING lines into README.md"
