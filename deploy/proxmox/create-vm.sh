#!/usr/bin/env bash
# create-vm.sh — create the ProteOS KVM-host VM on a Proxmox (PVE) host.
#
# Automates the one manual step in spike/firecracker/00-proxmox-vm.md: a VM with
# nested KVM so the guest gets /dev/kvm and Firecracker can run. Builds it from
# the Ubuntu 24.04 cloud image with cloud-init, so it boots SSH-ready and the
# Ansible playbook (deploy/ansible/) can configure it immediately.
#
# RUN THIS ON THE PROXMOX HOST (it uses `qm`/`pvesh`), as root.
#
#   sudo TEMPLATE=9000 ./create-vm.sh                  # clone from template 9000
#   sudo TEMPLATE=9000 VMID=9101 VM_NAME=fc-node-2 ./create-vm.sh
#   sudo TEMPLATE=9000 VM_IPCONFIG='ip=10.0.0.21/24,gw=10.0.0.1' ./create-vm.sh
#   sudo ./create-vm.sh                                # no TEMPLATE: build from cloud image
#
# Config is via environment variables (shown with defaults below). The script is
# safe to read first; it makes no changes until after the preflight checks.
set -euo pipefail

# --- config (override via env) ----------------------------------------------
VMID="${VMID:-9100}"
VM_NAME="${VM_NAME:-proteos-fc-node}"
VM_CORES="${VM_CORES:-4}"            # 00-proxmox-vm.md: 4 cores
VM_MEMORY="${VM_MEMORY:-8192}"       # 00-proxmox-vm.md: 8 GB
VM_DISK="${VM_DISK:-50G}"            # 00-proxmox-vm.md: 50 GB
VM_BRIDGE="${VM_BRIDGE:-vmbr0}"      # default bridge (VM needs internet)
VM_STORAGE="${VM_STORAGE:-local-lvm}"
VM_CPU="${VM_CPU:-host}"             # REQUIRED for nested KVM — do not change
VM_CIUSER="${VM_CIUSER:-ivan}"       # cloud-init login user (matches inventory)
# SSH pubkey(s) injected for VM_CIUSER. Defaults to root's authorized_keys on the
# PVE host, else ~/.ssh/id_*.pub. Override with VM_SSHKEYS=/path/to/keys.
VM_SSHKEYS="${VM_SSHKEYS:-}"
# Networking: DHCP by default. For a static lease set e.g.
#   VM_IPCONFIG='ip=10.0.0.21/24,gw=10.0.0.1'
VM_IPCONFIG="${VM_IPCONFIG:-ip=dhcp}"

# Source: clone an existing template (preferred) OR build from the cloud image
# (fallback when TEMPLATE is empty). TEMPLATE is a numeric template VMID; that
# template is expected to carry qemu-guest-agent + a cloud-init drive.
#   sudo TEMPLATE=9000 ./create-vm.sh
TEMPLATE="${TEMPLATE:-}"
FULL_CLONE="${FULL_CLONE:-1}"        # 1 = independent full clone; 0 = linked clone

# Ubuntu 24.04 LTS (noble) cloud image — x86_64, matches the rootfs line. Used
# only in the fallback (no-TEMPLATE) path.
IMG_URL="${IMG_URL:-https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img}"
IMG_CACHE="${IMG_CACHE:-/var/lib/vz/template/iso}"
IMG_FILE="$IMG_CACHE/noble-server-cloudimg-amd64.img"

START_VM="${START_VM:-1}"            # set 0 to create but not boot
ENABLE_NESTED="${ENABLE_NESTED:-0}"  # set 1 to persist kvm nested=1 if it's off

fail() { echo "[fail] $*" >&2; exit 1; }
ok()   { echo "[ ok ] $*"; }
log()  { echo "[info] $*"; }

# --- preflight ---------------------------------------------------------------
[[ $EUID -eq 0 ]] || fail "must run as root on the Proxmox host"
command -v qm  >/dev/null || fail "'qm' not found — run this ON the Proxmox host (PVE)"
command -v pvesm >/dev/null || fail "'pvesm' not found — is this a Proxmox host?"

# Nested virtualization: without it the guest has no /dev/kvm and nothing in
# ProteOS works. Check the loaded kvm module's `nested` param.
nested_param() {
  for m in kvm_intel kvm_amd; do
    if [[ -r /sys/module/$m/parameters/nested ]]; then
      echo "$m:$(cat /sys/module/$m/parameters/nested)"; return 0
    fi
  done
  return 1
}
if np=$(nested_param); then
  mod="${np%%:*}"; val="${np##*:}"
  if [[ $val == Y || $val == 1 ]]; then
    ok "nested virtualization enabled ($mod=$val)"
  elif [[ $ENABLE_NESTED == 1 ]]; then
    log "enabling nested virt: $mod nested=1 (persisted; reloading module)"
    echo "options $mod nested=1" >"/etc/modprobe.d/kvm-nested.conf"
    modprobe -r "$mod" 2>/dev/null || fail "cannot reload $mod (VMs running?) — reboot the host, then re-run"
    modprobe "$mod"
    ok "nested virt enabled for $mod"
  else
    fail "nested virt is OFF ($mod=$val). Re-run with ENABLE_NESTED=1, or set
      'options $mod nested=1' in /etc/modprobe.d/ and reboot. See 00-proxmox-vm.md"
  fi
else
  log "could not read kvm nested param — proceeding (CPU type 'host' still required)"
fi

# Refuse to clobber an existing VMID.
if qm status "$VMID" >/dev/null 2>&1; then
  fail "VMID $VMID already exists. Pick another (VMID=...) or 'qm destroy $VMID' first."
fi

# If cloning, the source template must exist, actually be a template, and differ
# from the target VMID.
if [[ -n $TEMPLATE ]]; then
  [[ $TEMPLATE != "$VMID" ]] || fail "TEMPLATE ($TEMPLATE) and VMID ($VMID) must differ"
  qm config "$TEMPLATE" >/dev/null 2>&1 || fail "template VMID $TEMPLATE not found"
  if ! qm config "$TEMPLATE" | grep -q '^template:\s*1'; then
    log "warning: VMID $TEMPLATE is not marked as a template — cloning it anyway"
  fi
  ok "cloning from template $TEMPLATE"
fi

# Resolve SSH keys.
if [[ -z $VM_SSHKEYS ]]; then
  for c in /root/.ssh/authorized_keys "$HOME/.ssh/id_ed25519.pub" "$HOME/.ssh/id_rsa.pub"; do
    [[ -s $c ]] && { VM_SSHKEYS="$c"; break; }
  done
fi
[[ -s ${VM_SSHKEYS:-} ]] || fail "no SSH pubkey found — set VM_SSHKEYS=/path/to/key.pub (needed for Ansible access)"
ok "ssh keys: $VM_SSHKEYS"

# --- create the VM -----------------------------------------------------------
if [[ -n $TEMPLATE ]]; then
  # ---- clone path (preferred): template already has guest-agent + cloud-init --
  clone_args=(--name "$VM_NAME")
  [[ $FULL_CLONE == 1 ]] && clone_args+=(--full --storage "$VM_STORAGE")
  log "cloning template $TEMPLATE -> VM $VMID ($VM_NAME)"
  qm clone "$TEMPLATE" "$VMID" "${clone_args[@]}"

  # Enforce the ProteOS shape on the clone (template values may differ). cpu=host
  # is the non-negotiable one — without it the guest gets no nested /dev/kvm.
  log "applying shape: ${VM_CORES} vCPU / ${VM_MEMORY}MiB / cpu=$VM_CPU"
  qm set "$VMID" --cpu "$VM_CPU" --cores "$VM_CORES" --memory "$VM_MEMORY" --agent enabled=1

  # Grow the boot disk to VM_DISK. Detect the boot disk from the config rather
  # than assuming scsi0 (qm disk resize only grows; a no-op/ shrink just warns).
  bootdisk="$(qm config "$VMID" | sed -n 's/^boot:.*order=\([a-z0-9]*\).*/\1/p' | head -n1)"
  bootdisk="${bootdisk:-scsi0}"
  log "resizing boot disk $bootdisk to $VM_DISK"
  qm disk resize "$VMID" "$bootdisk" "$VM_DISK" \
    || log "warning: resize of $bootdisk to $VM_DISK skipped (already >= that size?)"

  # The template carries a cloud-init drive; add one only if it somehow doesn't.
  if ! qm config "$VMID" | grep -q 'cloudinit'; then
    log "template had no cloud-init drive — adding one"
    qm set "$VMID" --ide2 "$VM_STORAGE:cloudinit"
  fi
else
  # ---- fallback path: build from the Ubuntu cloud image ----------------------
  mkdir -p "$IMG_CACHE"
  if [[ ! -f $IMG_FILE ]]; then
    log "downloading Ubuntu 24.04 cloud image -> $IMG_FILE"
    command -v wget >/dev/null || fail "'wget' not found (apt install wget)"
    wget -q --show-progress -O "$IMG_FILE.partial" "$IMG_URL"
    mv "$IMG_FILE.partial" "$IMG_FILE"
  fi
  ok "cloud image: $IMG_FILE"

  log "creating VM $VMID ($VM_NAME): ${VM_CORES} vCPU / ${VM_MEMORY}MiB / ${VM_DISK} / cpu=$VM_CPU"
  qm create "$VMID" \
    --name "$VM_NAME" \
    --cores "$VM_CORES" \
    --memory "$VM_MEMORY" \
    --cpu "$VM_CPU" \
    --machine q35 \
    --ostype l26 \
    --scsihw virtio-scsi-single \
    --net0 "virtio,bridge=$VM_BRIDGE" \
    --agent enabled=1 \
    --serial0 socket --vga serial0    # cloud images expect a serial console

  # Import the cloud image as scsi0 (PVE 8 one-step import-from).
  log "importing disk into $VM_STORAGE"
  qm set "$VMID" --scsi0 "$VM_STORAGE:0,import-from=$IMG_FILE"
  qm set "$VMID" --boot order=scsi0
  qm disk resize "$VMID" scsi0 "$VM_DISK"
  qm set "$VMID" --ide2 "$VM_STORAGE:cloudinit"
fi

# Cloud-init identity + network (both paths).
qm set "$VMID" --ciuser "$VM_CIUSER" --sshkeys "$VM_SSHKEYS" --ipconfig0 "$VM_IPCONFIG"

ok "VM $VMID created"

if [[ $START_VM == 1 ]]; then
  log "starting VM $VMID"
  qm start "$VMID"
  if [[ $VM_IPCONFIG != ip=dhcp ]]; then
    ok "started — reachable at: ${VM_IPCONFIG#ip=}"
  elif [[ -n $TEMPLATE ]]; then
    # Template carries qemu-guest-agent, so the agent can report the lease.
    ok "started — get its IP with: qm guest cmd $VMID network-get-interfaces"
  else
    # The stock cloud image has no guest agent yet; use DHCP leases or a static IP.
    ok "started — VM will DHCP; find its lease on your router/DHCP server"
  fi
else
  log "not starting (START_VM=0). Boot later with: qm start $VMID"
fi

cat <<EOF

Next:
  1. Wait for first boot + cloud-init, then SSH in as '$VM_CIUSER'.
  2. Point deploy/ansible/inventory.ini at this VM's IP.
  3. cd deploy/ansible && ansible-playbook -i inventory.ini site.yml \\
       --extra-vars "proteos_agent_token=\$(openssl rand -hex 32)"
EOF
