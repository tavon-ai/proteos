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
- `/etc/proteos-release` — base image, agent version, **feature set**, and build
  timestamp.

**Phase 4 (persistent disk + resume).** The baked guest agent now also brings up
per-machine persistence at startup, before any shell spawns (decision #7): it
waits for the node-agent-attached disk (`/dev/vdb`), `fsck`'s it, mounts it at
`/persist`, bind-mounts `/persist/home → /root` and `/persist/workspace →
/workspace` (so `$HOME` + the workspace live on the encrypted disk), and opens
the machine SQLite at `/persist/machine.db`. A missing disk degrades to ephemeral
rather than refusing terminals. It also serves the host-driven `PUT /resume`
(clock + entropy reseed after a snapshot restore) and `GET /info`. The agent runs
as **root** (no `User=` in the unit) so it can mount and `clock_settime`. No image
change is needed beyond rebuilding from this commit — the binary is built from
source — but the resulting image is a new `ga<gitshort>` ref, so **re-pin it**.

**Phase 5 (provider secrets + Claude Code).** The build also bakes:

- `/etc/profile.d/proteos-providers.sh` (`profile.d-proteos-providers.sh`) — sources
  the runtime-injected `/run/proteos/env/*.env` (tmpfs, 0600) into login shells so
  provider CLIs see their keys; typing `claude` in any terminal just works. Baked
  unconditionally; a no-op when nothing is injected.
- **Optionally** the pinned Claude Code CLI at `/usr/local/bin/claude` plus fleet
  managed settings at `/etc/claude-code/managed-settings.json` (`claude-managed-settings.json`,
  which disables the in-VM auto-updater so the image stays at its pinned version).
  Pass `--claude-binary` to include it; the version + sha256 are recorded in
  `manifest.lock` (`claude_version` / `claude_sha256`) and `/etc/proteos-release`.

The Anthropic API **key is never baked** — it is injected at runtime by the
control plane (Phase 5 decision #7). Per-user `~/.claude*` state lands in `$HOME`,
which is bind-mounted from the persistent disk (Phase 4), so a one-time first-run
approval persists across stop/start. Pre-answering that approval fully is verified
on the live host (RUNBOOK Part E / Phase 5 task 5.7) — it is exactly the CLI detail
the plan flags as drift-prone.

**Guest dev tooling (vim, Go, Taskfile) + shell customisation.** The build also
bakes a small dev toolchain into the guest, all **on by default** (`--no-vim` /
`--no-go` / `--no-taskfile` opt out):

- **vim** — installed extract-only via apt (the same resume-safe `dpkg-deb -x`
  discipline as git: the slimmed base has no dpkg metadata, so a normal install
  would reinstall the base closure and break Phase 4 resume).
- **Go** — the pinned toolchain tarball from go.dev (sha256-verified), unpacked
  into `/usr/local/go`. Pinned to the host Go version; override with
  `--go-version X.Y.Z`. Adds ~600 MiB to the image.
- **Taskfile** — the pinned `go-task` release (sha256-verified), installed as
  `/usr/local/bin/task`. Override the tag with `--taskfile-version vX.Y.Z`.
- `/etc/profile.d/proteos-shell.sh` — a managed snippet that puts Go on `PATH`
  (`/usr/local/go/bin` + `$GOPATH/bin`) and defines any operator aliases. It is
  sourced from `/root/.bashrc`, `/etc/skel/.bashrc`, and the run-as user's
  `~/.bashrc`, so it applies to both login and non-login interactive shells.

Bake shell aliases / extra `.bashrc` content with the repeatable flags:

```sh
image/build-rootfs.sh --base … \
  --alias 'll=ls -alF' --alias 'gs=git status' \
  --bashrc-file ./extra.bashrc
```

Through Ansible these map to `proteos_guest_{vim,go,taskfile}_install`,
`proteos_guest_{go,taskfile}_version`, and `proteos_guest_aliases` (a
name⇒command map) in `deploy/ansible/group_vars/all.yml`.

Output: `proteos-rootfs-<base>-ga<gitshort>.ext4` and `manifest.lock` (the
sha256 + provenance + feature set + Claude pin — committed).

## Building (Linux only)

The script loop-mounts ext4, so it needs a Linux kernel + `sudo`. Run it on the
Proxmox host (or any Linux box):

```sh
# Guest agent + providers wiring only:
image/build-rootfs.sh --base /path/to/firecracker-ci-ubuntu-24.04.ext4

# With Claude Code, fetched from Anthropic's official release endpoint and
# verified against the published manifest checksum (needs network on the build
# host; --claude-version pins the channel/version, default "stable"):
image/build-rootfs.sh --base /path/to/firecracker-ci-ubuntu-24.04.ext4 \
  --claude-bootstrap --claude-version stable

# Air-gapped alternative: bake a pre-fetched pinned binary. Obtain it on any
# networked box via Anthropic's installer, e.g.:
#   curl -fsSL https://downloads.claude.ai/claude-code-releases/bootstrap.sh | bash -s <version>
#   cp "$(command -v claude)" ./claude-<version> && sha256sum ./claude-<version>
image/build-rootfs.sh --base /path/to/firecracker-ci-ubuntu-24.04.ext4 \
  --claude-binary ./claude-<version> --claude-version <version> \
  --claude-sha256 <hex>
```

Then copy the resulting `.ext4` into the node-agent's images dir
(`PROTEOS_AGENT_IMAGES_DIR`), point the machine's `rootfs_ref`
(`PROTEOS_ROOTFS_REF` on the control plane) at its filename, and **re-pin** —
baking Claude Code yields a new image, so it is a new `ga<gitshort>` ref.

## Trust boundary

The guest agent listens on vsock with **no app-layer credential this phase**
(decision #10): the node-agent reaches it only through the per-VM jailed uds,
which is readable only by host root — "authenticated, not just private" holds by
construction. Per-machine identity (OpenBao) is deferred to Phase 7 (Phase 5
decision #8: it authenticates guest-initiated calls, of which Phase 5 has none —
its secret injection is a control-plane push). Phase 5 secret injection itself
uses that same push (the control plane PUTs `/secrets` over the tunnel), so the
guest needs no new credential.

## Verifying on a running VM

After the node-agent boots a VM from this rootfs:

```sh
# inside the guest (serial console):
systemctl status proteos-guestagent     # active (running)
cat /etc/proteos-release                 # FEATURES lists persist,resume,providers[,claude][,vim,go,taskfile]
findmnt /persist                         # the disk is mounted (Phase 4)
findmnt /root                            # bind-mounted from /persist/home
ls /persist                              # home/ workspace/ machine.db

# Phase 5 (5.5 done-when):
claude --version                         # the pinned CLI runs as root (if baked)
ls /etc/profile.d/proteos-providers.sh   # the providers snippet is present
# a login shell sources injected env (simulate an injected key):
mkdir -p /run/proteos/env && printf "export ANTHROPIC_API_KEY=sk-demo\n" > /run/proteos/env/claude.env
bash -lc 'echo $ANTHROPIC_API_KEY'       # → sk-demo (profile.d sourced it)
rm /run/proteos/env/claude.env

# Guest dev tooling:
vim --version | head -1                  # vim is installed
bash -lc 'go version'                     # Go on PATH (/usr/local/go/bin)
task --version                            # Taskfile CLI
bash -ic 'alias'                          # any baked --alias entries are present

# from the host: DialGuest reaches it through the jailed uds — see the
# `-tags=firecracker` integration test (nodeagent .../firecracker) and Task 3.7.
# A persisted file survives stop/start:
#   echo hi > ~/proof.txt   →  Stop (hibernate)  →  Start  →  cat ~/proof.txt
```
