# 00 — Create the Proxmox dev VM (manual, documented)

The only fully manual step of the spike. Everything after this is script-driven.
Record what you actually used in the Findings section of `README.md`.

## On the Proxmox host (once per cluster)

Nested virtualization must be enabled so the dev VM gets `/dev/kvm`:

```bash
# Intel:
cat /sys/module/kvm_intel/parameters/nested    # want: Y or 1
# AMD:
cat /sys/module/kvm_amd/parameters/nested      # want: 1
```

If it's off (Intel shown; use `kvm_amd` on AMD):

```bash
echo "options kvm_intel nested=1" > /etc/modprobe.d/kvm-nested.conf
modprobe -r kvm_intel && modprobe kvm_intel
```

## Create the VM

| Setting   | Value                                                  |
| --------- | ------------------------------------------------------ |
| OS        | Ubuntu Server 24.04 LTS (x86_64)                       |
| CPU       | 4 cores, **Type: `host`** ← required for nested KVM    |
| Memory    | 8 GB                                                   |
| Disk      | 50 GB                                                  |
| Network   | default bridge (VM needs internet for downloads)       |

CPU type `host` is the setting people forget — the default (`x86-64-v2-AES`
or `kvm64`) does **not** pass through virtualization extensions, and
Firecracker will fail with no `/dev/kvm`.

## Inside the VM (once)

```bash
sudo apt update && sudo apt install -y \
  curl wget tar squashfs-tools openssh-client screen acl iptables \
  e2fsprogs cpu-checker

# Verify nested KVM actually arrived:
kvm-ok               # want: "KVM acceleration can be used"
ls -l /dev/kvm       # must exist
```

If `kvm-ok` fails, go back to the CPU-type and nested-virt steps above —
nothing else in this spike will work until it passes.

## Then

Clone this repo inside the VM and run the numbered scripts in order, starting
with `./01-host-setup.sh`. See `README.md` for the full run order.
