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
| `tailscale`   | optional (off by default): add the apt repo, install `tailscale`, `tailscale up` with the supplied auth key; records the tailnet IP for the controller artifact |
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

## Outputs (controller artifacts)

After a run, the controlplane resources you'd otherwise SSH in and grep for are
written to `{{ proteos_templates_fetch_dir }}` (default `artifacts/`, gitignored)
— no more `ssh host sudo cat /etc/proteos/node-agent.env | grep ...`:

| File | Contents |
| ---- | -------- |
| `artifacts/node-<host>.env` | `PROTEOS_ROOTFS_REF`, `PROTEOS_AGENT_TOKEN`, `PROTEOS_KERNEL_REF`, `PROTEOS_NODE_ADDR` (the tailnet IP when Tailscale is on, else the inventory IP). **Holds the bearer token — mode 0600, never committed.** Source it or read the values into the control plane. |
| `artifacts/proteos-templates.json` | the control plane's `PROTEOS_TEMPLATES_FILE` (template catalog with baked image names). |

The final play also prints a `debug` summary with the file paths,
`PROTEOS_ROOTFS_REF`, and the node address.

Regenerate **just these artifacts** on an already-provisioned node (reads the
baked manifests, re-renders the catalog + `node-<host>.env`, fetches them back) —
no rebake, no service restart:

```bash
ansible-playbook -i inventory.ini site.yml --tags outputs \
  --extra-vars "proteos_agent_token=..."   # token still needed for node-<host>.env
```

## Tailscale (optional)

Off by default. Enable it to join the KVM host to a tailnet so the control plane
can reach the node-agent over a stable `100.x` address (survives the host's LAN IP
changing) — that address is exported as `PROTEOS_NODE_ADDR` in `node-<host>.env`.

```bash
# Generate a reusable/ephemeral auth key in the Tailscale admin console:
#   https://login.tailscale.com/admin/settings/keys
ansible-playbook -i inventory.ini site.yml \
  --extra-vars "proteos_agent_token=$(openssl rand -hex 32) \
                proteos_tailscale_enabled=true \
                proteos_tailscale_authkey=tskey-auth-..."
```

Better: keep `proteos_tailscale_authkey` in vault alongside the agent token. The
role adds the official apt repo, installs `tailscale`, and runs `tailscale up`
(idempotent — skipped when the backend is already `Running`). Knobs in
`group_vars/all.yml`: `proteos_tailscale_hostname` (default `inventory_hostname`),
`proteos_tailscale_ssh` (adds `--ssh`), `proteos_tailscale_up_args`
(e.g. `["--advertise-tags=tag:proteos"]`).

Run **just the Tailscale step** on an already-provisioned node with
`--tags tailscale` (still pass `proteos_tailscale_enabled=true` + the auth key).

## Key variables (`group_vars/all.yml`)

| Variable | Default | Notes |
| -------- | ------- | ----- |
| `proteos_agent_token` | `CHANGE_ME` | **required** — must equal app-stack token; the play refuses the placeholder |
| `proteos_fc_version` | `v1.16.0` | firecracker/jailer release |
| `proteos_kernel_key` | `firecracker-ci/v1.15/x86_64/vmlinux-6.1.176` | exact pinned kernel object |
| `proteos_go_version` | `1.26.4` | must satisfy `nodeagent/go.mod` |
| `proteos_agent_addr` | `0.0.0.0:9090` | listen address |
| `proteos_agent_subnet` | `172.30.0.0/24` | per-host guest subnet |
| `proteos_src_git_repo` | `""` | set to clone from git instead of rsync-ing this checkout |
| `proteos_restrict_agent_port` | `false` | set with `proteos_app_vm_ip` to lock `:9090` to the app VM |
| `proteos_run_acceptance_test` | `true` | run the KVM integration suite as a green-light gate before the service starts; auto-skips a node already serving machines |
| `proteos_agent_tls_cert`/`_key` | `""` | bring-your-own agent TLS cert; empty ⇒ the play self-signs one under `proteos_agent_tls_dir` and fetches it to `artifacts/node-<host>-agent-ca.pem` (TLS is mandatory — TAV-27) |
| `proteos_agent_mgmt_ifaces` | `"egress,tailscale0"` | interfaces allowed through the agent's fail-closed input chain; must include the CP's arrival path |
| `proteos_tailscale_enabled` | `false` | join the host to a tailnet; requires `proteos_tailscale_authkey` |
| `proteos_tailscale_authkey` | `""` | tailnet auth key (`tskey-...`); supply via vault/`--extra-vars` |

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
- **Machine templates:** the node_agent role bakes **one rootfs image per entry**
  in `proteos_templates` (default: `base`, `go`, `node`, `python`, `full`) —
  `proteos-rootfs-<id>-<base>-ga<sha>.ext4`, each with its own
  `manifest-<id>.lock`. It then renders `proteos-templates.json` (the control
  plane's `PROTEOS_TEMPLATES_FILE`) and fetches it to
  `{{ proteos_templates_fetch_dir }}` on the controller for the app-stack deploy
  to install. The platform layer (guest agent, git, vim, taskfile, code-server,
  dev user, Claude) is common to every template; `go`/`node`/`python` select the
  language layer. The npm provider CLIs (Gemini/Codex/pi.dev) ride on the Node
  layer, so they bake **only** into templates with `node: true`. Baking the full
  set is slow (~10–20 min each) — trim `proteos_templates` while iterating.
- **Forcing a rootfs rebuild:** delete the relevant `manifest-<id>.lock` under
  `/var/lib/proteos/images/` (one template), or all of them (the whole set); to
  also rebuild the base, delete `ubuntu-24.04.ext4` too. Re-run the playbook. A
  source change rebakes every template automatically (the git SHA is in each image
  name). The guest SSH key is preserved across rebuilds.
- **Baking Claude Code (Phase 5):** controlled by `proteos_claude_install`:
  - `bootstrap` (**default**) — the bake fetches the official `claude` native
    binary from Anthropic's release endpoint and verifies it against the published
    manifest checksum. Pin the channel/version with `proteos_claude_version`
    (`stable` default, or `latest`/`X.Y.Z`). Needs network on the **bake host**;
    the runtime image stays pinned/offline. The resolved version + sha256 are
    recorded in `manifest.lock`.
  - `binary` — air-gapped: provide a pre-fetched pinned binary via
    `proteos_claude_binary_url` *or* `proteos_claude_binary_src` (+ `proteos_claude_version`,
    and `proteos_claude_sha256` to pin), e.g.
    `--extra-vars 'proteos_claude_install=binary proteos_claude_version=2.1.89 proteos_claude_binary_src=./claude-2.1.89 proteos_claude_sha256=<hex>'`.
  - `none` — skip Claude Code (the providers `profile.d` wiring is still baked).
- **Baking the other providers (Phase 6):** Gemini, OpenAI Codex, and pi.dev are
  npm CLIs baked alongside a Node LTS runtime, each controlled by
  `proteos_<provider>_install` (`gemini`/`codex`/`pi`):
  - `none` (**default**) — these providers are **opt-in**; the base bake ships
    only Claude. Enable the ones you want, e.g.
    `--extra-vars 'proteos_gemini_install=latest proteos_codex_install=latest'`.
  - `latest` — install the CLI at its **latest** published version. Pin a specific
    release with `proteos_<provider>_version` (e.g. `proteos_gemini_version=0.4.1`).
    Like Claude bootstrap, this needs network on the **bake host**; the runtime
    image stays pinned. Node installs the latest LTS automatically when any of
    these is enabled (pin with `proteos_node_version`); the resolved versions land
    in `manifest.lock`.
  - Codex authenticates via a login step, wired automatically by the registry's
    `setup_command` (`codex login --with-api-key`) — no extra Ansible config.

  **Bumping the version/channel on an already-baked host needs a forced rebuild**
  (delete the per-template `manifest-<id>.lock` files), since the bake is guarded
  on source changes, not on these vars.
- The optional port firewall uses its **own** nft table + a oneshot unit, so it
  never clobbers the ruleset the node-agent manages for guest taps.
