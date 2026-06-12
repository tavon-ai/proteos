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
| 08   | `08-vsock.sh`            | virtio-vsock: guest listener on port 1024, host CONNECT/OK handshake echo; plain + jailed + across snapshot/restore (Phase 3 Task 3.0) |
| 09   | `09-encrypted-disk.sh`   | LUKS2 machine-volume mounted at `<chroot>/state`; rootfs+disk+snapshot all on it; encrypted hibernate→resume with a surviving process; no-plaintext grep; clock/CRNG findings (Phase 4 Task 4.0) |
| 10   | `10-measure-findings.sh` | Times the plain-path boot/snapshot/restore + jailer cgroup row and writes the `findings.{json,md}` artifacts that fill the table below (Phase 4 Task 4.0) |

Each script prints `[ ok ]` lines for what it verified and `[fail]` + exit ≠ 0
on the first thing that doesn't hold. Run them in order; 03–06 are
self-contained (each boots its own VM), so any one can be rerun in isolation
after 01.

## Spike acceptance (from the plan)

- [x] A second engineer reproduces the entire run on a fresh Proxmox VM using
      only this README and the scripts — no tribal knowledge.
- [ ] Boot, network, disk attach/persist, snapshot/restore, jailer all
      demonstrated.
- [ ] Clock-skew and entropy behavior after restore observed and recorded
      below (feeds Phase 4's resume criteria).
- [ ] Findings recorded below; they feed the driver design and the
      `firecracker-containerd` vs raw-API decision.

## Findings (fill in as you run)

> **Generated artifacts.** `./10-measure-findings.sh` measures the rows below and
> writes `findings.json` + `findings.md` (with host/version metadata for
> reproducibility); `./09-encrypted-disk.sh` writes `encrypted-findings.{json,md}`
> for the Phase-4 LUKS path. Paste the generated `.md` tables in here, or read the
> committed `.json` for the attributable numbers. The table below is the fallback
> for a manual run.

| Measurement | Value | Notes |
| --- | --- | --- |
| Boot to login prompt (02) | 9564 ms | InstanceStart→'login:' on serial; 2 vCPU/1024 MiB; console polled at 0.5s |
| Snapshot create time (05) | 1006 ms | Full snapshot (memory + device state), paused VM |
| Snapshot mem file size (05) | 1024 MiB | ≈ 1024 MiB guest RAM = the storage cost of hibernate |
| Restore + resume time (05) | 514 ms | LoadSnapshot with resume_vm=true, mem_backend=File; tap pre-existing |
| Clock skew after restore (05) | 16 s | ≈ the 15s hibernated; nothing resets the wall clock → node-agent PUT /resume must resync (decision #9) |
| CRNG reseeded after restore? (05) | yes | [   12.242326] random: crng reseeded due to virtual machine fork |
| cgroup placement under jailer (06) | 0::/firecracker/measure | from /proc/<vmm-pid>/cgroup; cpu.weight=512 |

### vsock findings (Task 3.0)

From `08-vsock.sh` (run 2026-06-11) — these gate the Phase 3 FirecrackerDriver
and feed Phase 4 (snapshot/restore of the vsock device):

- [x] Plain boot: host↔guest echo over `CONNECT 1024` works. The host connects to
      the device's host-side uds, sends `CONNECT 1024\n`, gets `OK <port>\n`, then
      bytes round-trip to a guest `AF_VSOCK` listener. Guest kernel/python have
      vsock support out of the box on the pinned CI rootfs.
- [ ] Jailed boot: **not executed by 08 directly** — documented as a manual add to
      `06-jailer.sh` (PUT `/vsock {uds_path:"v.sock"}` before InstanceStart). The
      uds lands inside the chroot at `<chroot>/root/v.sock`; still to confirm on a
      jailed run that it is owned by the jail uid (not root) and that host echo
      round-trips against that path. **Action: run the jailed variant before the
      3.7 acceptance pass.**
- [x] Snapshot/restore — **the host uds does NOT vanish on its own and is NOT
      reused in place.** After `kill_vm` the socket file is still `present` on
      disk; `LoadSnapshot` then tries to **re-bind** it and fails with
      "Address in use" unless it is removed first. So: the node-agent must
      `rm` the stale uds before restore, and Firecracker creates a **fresh** one
      on `LoadSnapshot` (post-restore echo confirmed working). Phase 4 must do
      this rm-then-restore.
- [x] In-flight connections across restore do **not** survive (the uds fd is
      per-VMM-process); the node-agent must redial after a restore. This matches
      the tunnel model (the gateway re-dials `DialGuest`).

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
