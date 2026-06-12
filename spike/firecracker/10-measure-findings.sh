#!/usr/bin/env bash
# 10 — Measure the Phase-4 findings-table values and emit committable artifacts.
#
# Runs the plain-path (non-jailed) boot → snapshot → restore cycle that 02/05
# describe, times each step, plus a short jailed boot for the 06 cgroup row, and
# writes two reproducible artifacts next to this script:
#
#   findings.json   machine-readable; carries env metadata (firecracker version,
#                   CI artifacts version, host kernel/cpu) so every number is
#                   attributable to the box + build it was measured on.
#   findings.md     the README "Findings" table, pre-filled — paste it in.
#
# Encrypted-path numbers (LUKS open/snapshot-on-volume, no-plaintext) come from
# 09-encrypted-disk.sh, which records its own encrypted-findings.* the same way.
#
# Rerunnable like the rest: tears down any previous VM first. Override the
# hibernate dwell (which the clock-skew row measures) with SKEW_SLEEP=<seconds>.
# Run after 01.

cd "$(dirname "${BASH_SOURCE[0]}")"
source ./env.sh
source ./lib.sh

require curl screen ssh python3 stat

[[ -f $KERNEL && -f $ROOTFS ]] || die "kernel/rootfs missing — run ./01-host-setup.sh first"

SKEW_SLEEP="${SKEW_SLEEP:-15}"               # hibernate dwell; the skew row ≈ this
ARTIFACT_DIR="${ARTIFACT_DIR:-$(pwd)}"        # repo spike dir → committable
FINDINGS_TSV="$RUN_DIR/findings.tsv"
finding_reset

# ==============================================================================
# Boot to login prompt (02)
# ==============================================================================
kill_vm
setup_network                                 # tap persists across the cycle (same name)
start_firecracker
put_machine_config
put_boot_source "$NET_BOOT_ARGS"
put_rootfs
put_network
boot_t0=$(date +%s%3N)
start_instance
wait_for_boot                                 # greps serial console for 'login:'
finding_set "Boot to login prompt (02)" "$(( $(date +%s%3N) - boot_t0 )) ms" \
  "InstanceStart→'login:' on serial; ${VCPUS} vCPU/${MEM_MIB} MiB; console polled at 0.5s"
wait_for_ssh

# Dirty some RAM + run a heartbeat so the snapshot reflects a realistic guest and
# the restored process is verifiable (we don't assert it here — 05/09 do — but it
# keeps the mem-file size honest rather than a freshly-booted near-zero image).
guest_ssh "nohup sh -c 'while true; do date >> /tmp/hb; sleep 1; done' >/dev/null 2>&1 & sleep 0.2"
sleep 2

# ==============================================================================
# Snapshot create time + mem file size (05)
# ==============================================================================
mkdir -p "$SNAPSHOT_DIR"
guest_dmesg_baseline                           # clear ring buffer → unambiguous reseed detection post-restore
fc_api PATCH /vm '{"state": "Paused"}'
snap_t0=$(date +%s%3N)
fc_api PUT /snapshot/create "{\"snapshot_type\":\"Full\",
  \"snapshot_path\":\"$SNAPSHOT_DIR/vmstate\",\"mem_file_path\":\"$SNAPSHOT_DIR/mem\"}"
finding_set "Snapshot create time (05)" "$(( $(date +%s%3N) - snap_t0 )) ms" \
  "Full snapshot (memory + device state), paused VM"
finding_set "Snapshot mem file size (05)" "$(( $(stat -c%s "$SNAPSHOT_DIR/mem") / 1048576 )) MiB" \
  "≈ ${MEM_MIB} MiB guest RAM = the storage cost of hibernate"

# ==============================================================================
# Restore + resume time (05) + clock skew after restore
# ==============================================================================
kill_vm
log "sleeping ${SKEW_SLEEP}s so post-restore clock skew is observable"
sleep "$SKEW_SLEEP"
restore_t0=$(date +%s%3N)
start_firecracker
fc_api PUT /snapshot/load "{\"snapshot_path\":\"$SNAPSHOT_DIR/vmstate\",
  \"mem_backend\":{\"backend_type\":\"File\",\"backend_path\":\"$SNAPSHOT_DIR/mem\"},
  \"resume_vm\":true}"
finding_set "Restore + resume time (05)" "$(( $(date +%s%3N) - restore_t0 )) ms" \
  "LoadSnapshot with resume_vm=true, mem_backend=File; tap pre-existing"
wait_for_ssh

guest_now=$(guest_ssh 'date +%s'); host_now=$(date +%s)
finding_set "Clock skew after restore (05)" "$(( host_now - guest_now )) s" \
  "≈ the ${SKEW_SLEEP}s hibernated; nothing resets the wall clock → node-agent PUT /resume must resync (decision #9)"

# ==============================================================================
# CRNG reseeded after restore? (05) — does VMGenID trigger a reseed on this kernel
# ==============================================================================
vmgen=$(guest_vmgenid_probe)
finding_set "CRNG reseeded after restore? (05)" "${vmgen%%$'\t'*}" "${vmgen#*$'\t'}"
kill_vm

# ==============================================================================
# cgroup placement under jailer (06) — best-effort: --cgroup flags vary by distro
# ==============================================================================
# Only the cgroup string goes to stdout (it is captured by the caller); every
# helper that chatters to stdout — wait_for's `ok`, etc. — is redirected to
# stderr inside the group, so it stays visible as progress but never pollutes the
# captured value. (The earlier version leaked "[ ok ] jailed API socket" into the
# finding and the embedded newline split the TSV row.)
measure_cgroup() {
  local jid="measure" chroot sock pid
  chroot="$JAIL_DIR/firecracker/$jid/root"
  sock="$chroot/run/firecracker.socket"
  {
    id -u "$FC_USER" >/dev/null 2>&1 ||
      sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$FC_USER"
    sudo pkill -u "$FC_USER" firecracker 2>/dev/null || true
    sudo rm -rf "$JAIL_DIR/firecracker/$jid"
    sudo mkdir -p "$chroot/run"
    sudo cp "$KERNEL" "$chroot/vmlinux"
    sudo cp "$ROOTFS" "$chroot/rootfs.ext4"
    sudo chown -R "$FC_USER:$FC_USER" "$JAIL_DIR/firecracker/$jid"

    require_kvm
    sudo "$BIN_DIR/jailer" --id "$jid" --exec-file "$BIN_DIR/firecracker" \
      --uid "$(id -u "$FC_USER")" --gid "$(id -g "$FC_USER")" \
      --chroot-base-dir "$JAIL_DIR" --cgroup-version 2 --cgroup "cpu.weight=512" \
      --daemonize -- --api-sock /run/firecracker.socket || return 1
    wait_for "jailed API socket" 10 bash -c "sudo test -S '$sock'" || return 1

    local jc=(--unix-socket "$sock" -sS -f -H 'Content-Type: application/json')
    sudo curl "${jc[@]}" -X PUT "http://localhost/machine-config" -d '{"vcpu_count":1,"mem_size_mib":256}' || return 1
    sudo curl "${jc[@]}" -X PUT "http://localhost/boot-source" -d '{"kernel_image_path":"/vmlinux","boot_args":"console=ttyS0 reboot=k panic=1 pci=off"}' || return 1
    sudo curl "${jc[@]}" -X PUT "http://localhost/drives/rootfs" -d '{"drive_id":"rootfs","path_on_host":"/rootfs.ext4","is_root_device":true,"is_read_only":false}' || return 1
    sudo curl "${jc[@]}" -X PUT "http://localhost/actions" -d '{"action_type":"InstanceStart"}' || return 1

    pid=$(pgrep -u "$FC_USER" -f firecracker | head -n1)
    [[ -n $pid ]] || return 1
  } >&2
  sudo cat "/proc/$pid/cgroup" | tr '\n' ';'
}

# Run it isolated so a cgroup-flag failure records "unavailable" instead of
# aborting the whole artifact (06 already proves the jailer; this is just the row).
set +e
cg="$(measure_cgroup)"
cg_rc=$?
set -e
sudo pkill -u "$FC_USER" firecracker 2>/dev/null || true
if [[ $cg_rc -eq 0 && -n $cg ]]; then
  finding_set "cgroup placement under jailer (06)" "${cg%;}" "from /proc/<vmm-pid>/cgroup; cpu.weight=512"
else
  finding_set "cgroup placement under jailer (06)" "unavailable" \
    "jailer --cgroup-version 2 failed on this host; see 06-jailer.sh notes"
fi

# ==============================================================================
# Emit artifacts
# ==============================================================================
finding_finalize "$ARTIFACT_DIR/findings.json" "$ARTIFACT_DIR/findings.md" \
  "Firecracker spike findings — plain path (02/05/06)"
log "commit $ARTIFACT_DIR/findings.{json,md}; the .md table drops straight into README.md"
ok "10 complete — Phase-4 findings measured into reproducible artifacts"
