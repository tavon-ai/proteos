# ProteOS node-agent

The host-side agent that drives the VM backend — the **Firecracker driver** on a
KVM host, or a process-backed **dev driver** on a Mac. It serves the agent HTTP
API the control plane calls and bridges the in-guest terminal over vsock. See
`../CLAUDE.md` and `../plans/` for architecture.

Build / run with `../deploy/node-agent/run-node-agent.sh`, or provision a host
with `../deploy/ansible/`. Config fields are documented in
`../deploy/node-agent/.env.example`.

## Tests

`go test ./...` runs the unit + dev-driver suite anywhere — no KVM, no root.

### Gated KVM integration tests (`-tags firecracker`)

`internal/driver/firecracker/*_integration_test.go` boot **real** jailed
microVMs, so they need a Linux **KVM host** (the Proxmox `fc-node`), **root**, the
firecracker/jailer binaries, **cryptsetup**, a pinned kernel, and a **baked**
rootfs — the plain Firecracker-CI base has no guest agent, so the resume-hygiene
proof needs the image produced by `../image/build-rootfs.sh`. Without those env
vars every test `t.Skip`s, so a normal `go test ./...` ignores them.

`TestHibernateResumeCycle` is the Phase 4 acceptance proof: cold boot → hibernate
(Full snapshot onto the encrypted LUKS volume) → resume, asserting the disk is
**encrypted at rest** (a plaintext probe written to the open volume is absent
from the raw, closed `.luks`; the container is `cryptsetup isLuks`) and that
**resume hygiene ran** (the guest `/resume` hook resynced the clock + reseeded
entropy → `Status.ResumeHygiene == "ok"`). The hygiene assertion only fires when
you point at a guest-agent rootfs and set `PROTEOS_TEST_ROOTFS_HAS_GUEST_AGENT=1`.

Bake the rootfs once (`../image/build-rootfs.sh`), then run the whole suite from a
checkout on the KVM host, as root:

```bash
cd nodeagent
sudo env "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/sbin:/sbin:/usr/bin:/bin" \
  GOWORK=off \
  PROTEOS_TEST_KERNEL=/var/lib/proteos/images/vmlinux \
  PROTEOS_TEST_ROOTFS=/var/lib/proteos/images/<baked-rootfs>.ext4 \
  PROTEOS_TEST_ROOTFS_HAS_GUEST_AGENT=1 \
  PROTEOS_CRYPTSETUP_BIN=/usr/sbin/cryptsetup \
  go test -tags firecracker -count=1 -v ./internal/driver/firecracker/
```

| Env var | Required | Default | Purpose |
| --- | --- | --- | --- |
| `PROTEOS_TEST_KERNEL` | yes | — | path to the `vmlinux` |
| `PROTEOS_TEST_ROOTFS` | yes | — | path to the **baked** ext4 rootfs |
| `PROTEOS_TEST_ROOTFS_HAS_GUEST_AGENT` | no | unset | set `1` to enforce the resume-hygiene assertion (needs the baked rootfs) |
| `PROTEOS_FIRECRACKER_BIN` / `PROTEOS_JAILER_BIN` | no | `/usr/local/bin/*` | binary locations (a spike host has them under `~/fc-spike/bin`) |
| `PROTEOS_CRYPTSETUP_BIN` | no | `cryptsetup` (PATH) | LUKS tool |

> On a **spike-provisioned** host the kernel/rootfs live under `~/fc-spike/images`
> and the binaries under `~/fc-spike/bin` — point the vars there and add the
> `PROTEOS_FIRECRACKER_BIN` / `PROTEOS_JAILER_BIN` overrides.

The Ansible playbook runs this suite as a **provisioning gate** (see
`../deploy/ansible/`, `proteos_run_acceptance_test`), so a node is not green-lit
unless the encrypted hibernate/resume cycle works on its own hardware. The gate
auto-skips a node that is already serving machines (IP/tap contention with live
VMs) — run it on a fresh node, or `--skip-tags acceptance`.
```

