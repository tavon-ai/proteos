# ProteOS KVM-host VM — Proxmox

`create-vm.sh` automates the one manual step in
`spike/firecracker/00-proxmox-vm.md`: creating the Proxmox VM that runs the
ProteOS node-agent. It produces a cloud-init Ubuntu 24.04 VM with the required
shape (4 vCPU, **`cpu host`**, 8 GB, 50 GB) so it boots SSH-ready, then the
Ansible playbook in `deploy/ansible/` configures it.

Two source paths:

- **Clone a template** (preferred) — `TEMPLATE=<vmid>`. Fast, and the template
  already carries `qemu-guest-agent` (so Proxmox can report the VM's IP). The
  script still re-applies `cpu host`, cores, memory, and resizes the disk so the
  clone always matches the required shape regardless of the template's settings.
- **Build from the cloud image** (fallback) — no `TEMPLATE`. Downloads the
  Ubuntu 24.04 cloud image and imports it.

## End-to-end

```
deploy/proxmox/create-vm.sh   →   deploy/ansible/site.yml
   (on the PVE host)                (from your workstation)
   makes the VM                     installs firecracker + node-agent
```

## Run (on the Proxmox host, as root)

```bash
# clone from template 9000 (preferred):
sudo TEMPLATE=9056 VMID=7115 VM_STORAGE=black VM_SSHKEYS=ivan-mac.pub ./create-vm.sh
# override anything:
sudo TEMPLATE=9000 VMID=9101 VM_NAME=fc-node-2 VM_STORAGE=local-lvm \
     VM_IPCONFIG='ip=10.0.0.21/24,gw=10.0.0.1' ./create-vm.sh
# no template -> build from the cloud image instead:
sudo ./create-vm.sh
```

The script:
- checks **nested virtualization** is on (re-run with `ENABLE_NESTED=1` to
  persist `kvm_intel/kvm_amd nested=1` if it's off — needs no running VMs, or a
  reboot),
- refuses to clobber an existing VMID,
- downloads the Ubuntu 24.04 cloud image (cached under `/var/lib/vz/template/iso`),
- creates the VM with `cpu host` + cloud-init (user, your SSH key, DHCP or a
  static `VM_IPCONFIG`), and starts it.

## Key variables

| Var | Default | Notes |
| --- | ------- | ----- |
| `TEMPLATE` | _(empty)_ | template VMID to clone; empty = build from cloud image |
| `FULL_CLONE` | `1` | `1` full (independent) clone, `0` linked clone |
| `VMID` | `9100` | must be free, and differ from `TEMPLATE` |
| `VM_NAME` | `proteos-fc-node` | |
| `VM_CORES` / `VM_MEMORY` / `VM_DISK` | `4` / `8192` / `50G` | per `00-proxmox-vm.md` |
| `VM_CPU` | `host` | **required** for nested KVM — don't change |
| `VM_STORAGE` | `local-lvm` | target storage for disk + cloudinit |
| `VM_BRIDGE` | `vmbr0` | needs internet for the playbook's downloads |
| `VM_CIUSER` | `ivan` | cloud-init login user (match the Ansible inventory) |
| `VM_SSHKEYS` | root's `authorized_keys`, else `~/.ssh/id_*.pub` | injected pubkey |
| `VM_IPCONFIG` | `ip=dhcp` | e.g. `ip=10.0.0.21/24,gw=10.0.0.1` for static |
| `ENABLE_NESTED` | `0` | set `1` to persist+load `kvm nested=1` if off |
| `START_VM` | `1` | set `0` to create but not boot |

## After it boots

Find the VM's IP. When cloning a template that has qemu-guest-agent:

```bash
qm guest cmd <VMID> network-get-interfaces
```

(Or just use a static `VM_IPCONFIG`. The cloud-image fallback has no guest agent,
so there fall back to a static IP or your DHCP server's lease table.)

```bash
# from your workstation:
cd ../ansible
cp inventory.example.ini inventory.ini   # point at the VM IP, set ansible_user
ansible-playbook -i inventory.ini site.yml \
  --extra-vars "proteos_agent_token=$(openssl rand -hex 32)"
```
