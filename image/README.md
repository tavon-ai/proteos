# ProteOS guest rootfs

The baked microVM root filesystem: the pinned Firecracker-CI Ubuntu base with the
ProteOS **guest agent** installed and enabled at boot. This is the Phase 3
implementation of decision #2 and the pinned manual seed of Phase 12's image
pipeline.

> Not to be confused with `../images/`, which holds the README screenshots.

## What gets baked in

`build-rootfs.sh` loop-mounts a copy of the pinned base ext4 and installs:

- `/usr/local/bin/guestagent` — the static (`CGO_ENABLED=0`) Linux guest agent,
  version-stamped via `-ldflags -X main.version`.
- `/etc/systemd/system/proteos-guestagent.service` (+ the
  `multi-user.target.wants` symlink so it starts at boot) — runs the agent on
  `vsock:1024` with a login `bash` shell (Phase 3 decision #6).
- `/etc/proteos-release` — base image, agent version, and build timestamp.

Output: `proteos-rootfs-<base>-ga<gitshort>.ext4` and `manifest.lock` (the
sha256 + provenance — committed).

## Building (Linux only)

The script loop-mounts ext4, so it needs a Linux kernel + `sudo`. Run it on the
Proxmox host (or any Linux box):

```sh
image/build-rootfs.sh --base /path/to/firecracker-ci-ubuntu-24.04.ext4
```

Then copy the resulting `.ext4` into the node-agent's images dir
(`PROTEOS_AGENT_IMAGES_DIR`) and point the machine's `rootfs_ref`
(`PROTEOS_ROOTFS_REF` on the control plane) at its filename.

## Trust boundary

The guest agent listens on vsock with **no app-layer credential this phase**
(decision #10): the node-agent reaches it only through the per-VM jailed uds,
which is readable only by host root — "authenticated, not just private" holds by
construction. Per-machine identity (OpenBao) arrives in Phase 5.

## Verifying on a running VM

After the node-agent boots a VM from this rootfs:

```sh
# inside the guest (serial console):
systemctl status proteos-guestagent     # active (running)
cat /etc/proteos-release

# from the host: DialGuest reaches it through the jailed uds — see the
# `-tags=firecracker` integration test (nodeagent .../firecracker) and Task 3.7.
```
