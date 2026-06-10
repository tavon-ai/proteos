# Firecracker spike (plan Task 2.0)

Scripted, reproducible run-through of every Firecracker capability the
node-agent driver will need. See `plans/proteos-poc-to-prod.md` → Phase 2 →
Task 2.0.

> **This code is throwaway.** Its output is the *findings* below and the
> confidence that each capability works on our infrastructure. Do not grow it
> into a mini node-agent — when a script starts wanting state or abstraction,
> stop and write the real driver instead.

## Reproducibility contract

- Versions are **pinned** in `env.sh`; `01-host-setup.sh` writes the exact
  resolved artifacts to `versions.lock` — commit it.
- Every script is rerunnable: each starts by killing any previous VM and
  checks-before-creates host resources (tap, iptables rules, images).
- The only manual step is creating the Proxmox VM, documented in
  `00-proxmox-vm.md`.
- All host state lives in `~/fc-spike` (override: `FC_SPIKE_WORK_DIR`);
  `./07-teardown.sh --all` removes everything.

## Run order

| Step | Script                   | Proves                                                        |
| ---- | ------------------------ | ------------------------------------------------------------- |
| 00   | `00-proxmox-vm.md`       | Proxmox VM with nested KVM (`kvm-ok` passes) — *manual, documented* |
| 01   | `01-host-setup.sh`       | Pinned firecracker/jailer/kernel/rootfs install; `/dev/kvm` access |
| 02   | `02-boot-vm.sh`          | API-driven configure + boot to a login prompt; serial console via `screen -r fc-spike` |
| 03   | `03-network.sh`          | tap + private IP + NAT: host→guest, SSH, guest→internet        |
| 04   | `04-attach-disk.sh`      | Second disk attach; file survives full stop + cold boot        |
| 05   | `05-snapshot-restore.sh` | Snapshot → kill VMM → restore in a fresh process; clock/entropy observations |
| 06   | `06-jailer.sh`           | Same boot under jailer: chroot + uid drop + cgroup, verified   |
| 07   | `07-teardown.sh`         | Clean slate (add `--all` to remove downloaded artifacts)       |

Each script prints `[ ok ]` lines for what it verified and `[fail]` + exit ≠ 0
on the first thing that doesn't hold. Run them in order; 03–06 are
self-contained (each boots its own VM), so any one can be rerun in isolation
after 01.

## Spike acceptance (from the plan)

- [ ] A second engineer reproduces the entire run on a fresh Proxmox VM using
      only this README and the scripts — no tribal knowledge.
- [ ] Boot, network, disk attach/persist, snapshot/restore, jailer all
      demonstrated.
- [ ] Clock-skew and entropy behavior after restore observed and recorded
      below (feeds Phase 4's resume criteria).
- [ ] Findings recorded below; they feed the driver design and the
      `firecracker-containerd` vs raw-API decision.

## Findings (fill in as you run)

| Measurement                            | Value | Notes |
| -------------------------------------- | ----- | ----- |
| Boot to login prompt (02)               |       |       |
| Snapshot create time + mem file size (05) |     | mem file ≈ RAM size = storage cost of hibernate |
| Restore + resume time (05)              |       |       |
| Clock skew after restore (05)           |       | expected ≈ hibernated duration; Phase 4 must resync |
| CRNG reseeded after restore? (05)       |       | `dmesg \| grep -i random` in the guest |
| cgroup placement under jailer (06)      |       |       |

### Surprises / gotchas log

Append anything that cost you time — this section is half the value of the spike.

- `unsquashfs` as non-root warns about device nodes it can't create; harmless
  (the guest mounts devtmpfs).
- If 02 times out waiting for `login:`, check `~/fc-spike/run/vm-console.log` —
  the kernel may have booted fine but the CI rootfs's getty config changed;
  SSH in 03 is the authoritative liveness check.
- NICs must be configured **before** `InstanceStart`; Firecracker cannot
  hot-add them.
- Restoring a snapshot requires the same Firecracker version that created it,
  and the tap device must exist with the same name.
- (add yours here)

### Driver-design takeaways

To fill in after the run — what the spike implies for the node-agent driver
interface and the `firecracker-containerd` vs raw-API decision (see plan →
Open decisions).

-
