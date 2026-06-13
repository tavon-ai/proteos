# ProteOS node-agent — Ansible

Reproducible configuration of a **Proxmox KVM host** to run the ProteOS
node-agent (Firecracker driver). This replaces the manual run of
`spike/firecracker/01-host-setup.sh` + `prod-setup.sh` +
`deploy/node-agent/setup-service.sh` with one idempotent playbook.

It does **not** configure the control-plane app-stack (db + control-plane +
web) — that runs on a separate VM, see `deploy/app-stack/`.

## What it does

| Role          | Actions |
| ------------- | ------- |
| `common`      | apt packages, assert `/dev/kvm`, persistent `net.ipv4.ip_forward=1`, create `/var/lib/proteos/{images,agent,volumes}` + `/srv/jailer` + `/etc/proteos` |
| `go`          | install pinned Go (`go.mod` needs 1.26.4) from the official tarball |
| `firecracker` | install pinned firecracker + jailer to `/usr/local/bin`, download the pinned CI kernel, build the ext4 rootfs with a generated SSH key |
| `node_agent`  | sync source to `/opt/proteos/src`, build the binary, bake the guest-agent rootfs, **run the KVM acceptance gate**, render `/etc/proteos/node-agent.env`, install + enable the systemd unit, optional port firewall |

The **acceptance gate** runs the firecracker integration suite (encrypted
hibernate/resume + boot/stop/egress/vsock) against the just-baked rootfs before
the service starts, so a node is never green-lit unless it can actually run
encrypted microVMs. It boots real VMs (~2 min) and auto-skips a node that is
already serving machines. Skip with `--skip-tags acceptance` or
`proteos_run_acceptance_test=false`; run it on its own with `--tags acceptance`.

Everything is pinned in `group_vars/all.yml` to match
`spike/firecracker/versions.lock`, so every host gets byte-identical artifacts.

## Prerequisites

- A Proxmox VM with **nested virt** (CPU type `host`) so `/dev/kvm` exists — see
  `spike/firecracker/00-proxmox-vm.md`. The playbook asserts this and fails
  early if it's missing.
- SSH access to the host as a sudo-capable user.
- On the control machine: `ansible` + the collections below.

```bash
ansible-galaxy collection install -r requirements.yml
cp inventory.example.ini inventory.ini   # then edit the host/IP/user
```

## Run

```bash
# Token must match the app stack's PROTEOS_AGENT_TOKEN.
ansible-playbook -i inventory.ini site.yml \
  --extra-vars "proteos_agent_token=$(openssl rand -hex 32)"
```

Better: keep the token in vault instead of on the command line.

```bash
ansible-vault create group_vars/vault.yml      # add: proteos_agent_token: "<hex>"
ansible-playbook -i inventory.ini site.yml --ask-vault-pass
```

Useful flags: `--check --diff` (dry run), `--tags`/`--start-at-task`,
`-l fc-node-1` (limit to one host).

## Key variables (`group_vars/all.yml`)

| Variable | Default | Notes |
| -------- | ------- | ----- |
| `proteos_agent_token` | `CHANGE_ME` | **required** — must equal app-stack token; the play refuses the placeholder |
| `proteos_fc_version` | `v1.16.0` | firecracker/jailer release |
| `proteos_kernel_key` | `firecracker-ci/v1.15/x86_64/vmlinux-6.1.155` | exact pinned kernel object |
| `proteos_go_version` | `1.26.4` | must satisfy `nodeagent/go.mod` |
| `proteos_agent_addr` | `0.0.0.0:9090` | listen address |
| `proteos_agent_subnet` | `172.30.0.0/24` | per-host guest subnet |
| `proteos_src_git_repo` | `""` | set to clone from git instead of rsync-ing this checkout |
| `proteos_restrict_agent_port` | `false` | set with `proteos_app_vm_ip` to lock `:9090` to the app VM |
| `proteos_run_acceptance_test` | `true` | run the KVM integration suite as a green-light gate before the service starts; auto-skips a node already serving machines |
| `proteos_agent_tls_cert`/`_key` | `""` | set both to serve the agent channel over TLS |

## Verify

```bash
ssh <host> 'systemctl status proteos-node-agent --no-pager'
ssh <host> 'journalctl -u proteos-node-agent -n 50 --no-pager'
ssh <host> 'curl -fsS -H "Authorization: Bearer <token>" http://127.0.0.1:9090/healthz'
```

## Notes / limitations

- **The token is the only required input.** Bumping a pinned version
  (firecracker, kernel, Go) and re-running upgrades the host in place; the
  firecracker/kernel/rootfs steps are guarded so unchanged artifacts are a no-op.
- **Forcing a rootfs rebuild:** delete `/var/lib/proteos/images/manifest.lock` (and,
  to also rebuild the base, `ubuntu-24.04.ext4`) on the host and re-run. The guest
  SSH key is preserved across rebuilds.
- **Baking Claude Code (Phase 5):** set `proteos_claude_version` and provide the
  pinned linux-x64 `claude` binary via `proteos_claude_binary_url` *or*
  `proteos_claude_binary_src` (+ `proteos_claude_sha256` to pin), e.g.
  `--extra-vars 'proteos_claude_version=2.1.89 proteos_claude_binary_src=./claude-2.1.89 proteos_claude_sha256=<hex>'`.
  The bake then installs `/usr/local/bin/claude`; the providers `profile.d` wiring
  is baked regardless. **Bumping the Claude version on an already-baked host needs a
  forced rebuild** (delete `manifest.lock`), since the bake is guarded on source
  changes, not on `proteos_claude_version`.
- The optional port firewall uses its **own** nft table + a oneshot unit, so it
  never clobbers the ruleset the node-agent manages for guest taps.
