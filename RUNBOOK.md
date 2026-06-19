# ProteOS — Full-stack runbook (Phase 2 acceptance)

How to run the complete Phase 2 stack and check off the acceptance criteria in
`plans/proteos-poc-to-prod.md`. This completes Task 2.8 ("run the full stack on
the Proxmox VM").

It is written to be **reproducible by someone else on different machines** —
every host-specific value is an env var with an `.env.example`, and the only
manual host step (the KVM VM) is the one already documented in
`spike/firecracker/00-proxmox-vm.md`.

## Topology

Two machines:

```
┌───────────────────────────┐         ┌──────────────────────────────────────┐
│ App VM (any Docker host)  │         │ KVM host = Proxmox VM (nested KVM)    │
│                           │  HTTP   │                                      │
│  docker compose:          │  :9090  │  native (root) systemd service:      │
│   postgres                │ ──────► │   proteos-node-agent                 │
│   controlplane  ──────────┼─────────┤   (Firecracker driver)               │
│   web (nginx + SPA)       │ bearer  │     └─ jailer ► microVMs (guests)    │
│   browser ⇒ :8080         │  token  │                                      │
└───────────────────────────┘         └──────────────────────────────────────┘
```

- The **node-agent cannot be containerised** — it needs `/dev/kvm`, root, tap
  devices, the jailer, and host nftables. It runs natively on the KVM host.
- Everything else (Postgres, control plane, web) is portable and ships as a
  Docker Compose stack on the **app VM**.
- The control plane dials the agent **only** (plan decision #2), authenticated
  by a shared bearer token. Status flows back via the control plane's poller.
- The browser talks to one origin (the `web` nginx); nginx serves the SPA and
  reverse-proxies `/api` to the control plane so cookies + CSRF work.

Artifacts: `deploy/node-agent/` (KVM host) and `deploy/app-stack/` (app VM).

> Can both run on the **same** Proxmox VM? Yes — run the node-agent natively and
> the compose stack on the same box; set `PROTEOS_NODE_AGENT_URL=http://host.docker.internal:9090`
> and uncomment `extra_hosts` in `compose.yml`. The two-VM split below is the
> documented path.

---

## Part A — node-agent on the KVM host

Prereqs: the Firecracker spike has been run on this VM (`spike/firecracker/01-host-setup.sh`),
so `/dev/kvm` works and the pinned `firecracker`+`jailer` binaries exist under
`~/fc-spike/bin`. Go is installed (you already ran the `-tags=firecracker` tests).

### A1. Stage the pinned images

The driver resolves `kernel_ref`/`rootfs_ref` as filenames under the images dir
(`filepath.Join(ImagesDir, ref)`), and makes a fresh writable rootfs copy per
boot — so the base files stay pristine. Reuse the spike artifacts:

```bash
sudo mkdir -p /var/lib/proteos/images /srv/jailer
sudo cp ~/fc-spike/images/vmlinux           /var/lib/proteos/images/vmlinux
sudo cp ~/fc-spike/images/ubuntu-24.04.ext4 /var/lib/proteos/images/ubuntu-24.04.ext4
```

### A2. Point the agent at the firecracker/jailer binaries

The spike leaves them in `~/fc-spike/bin`. Either copy them to `/usr/local/bin`
(the defaults), or set `PROTEOS_FIRECRACKER_BIN`/`PROTEOS_JAILER_BIN` in the env
file. Copying is cleaner for a service:

```bash
sudo cp ~/fc-spike/bin/firecracker ~/fc-spike/bin/jailer /usr/local/bin/
```

### A3. Configure + run

```bash
cd <repo>/deploy/node-agent
cp .env.example .env
# set PROTEOS_AGENT_TOKEN (openssl rand -hex 32 — save it, the app VM needs it)
# confirm the paths and PROTEOS_AGENT_SUBNET
$EDITOR .env

sudo ./run-node-agent.sh        # preflight, build (-tags=firecracker), run foreground
```

Preflight prints `[ ok ]` for kvm/firecracker/jailer/ip/nft/images, or `[fail]`
with the exact missing thing. From another shell:

```bash
curl -s 127.0.0.1:9090/healthz   # {"status":"ok"}
```

### A4. (Recommended) Run it as a service

Persist across reboots and capture logs — see the install block at the top of
`deploy/node-agent/proteos-node-agent.service`. In short: build the binary once
(`run-node-agent.sh build`), put the env file at `/etc/proteos/node-agent.env`,
install + `systemctl enable --now proteos-node-agent`, then
`journalctl -u proteos-node-agent -f`.

### A5. Open the port to the app VM only

The agent listens on `0.0.0.0:9090`; the bearer token authenticates callers, but
still scope it at the firewall:

```bash
sudo ufw allow from <APP_VM_IP> to any port 9090 proto tcp   # example
```

---

## Part B — app stack on the app VM

Prereq: Docker + Docker Compose v2. Clone the repo (or copy `deploy/app-stack/`
plus the `controlplane/`, `nodeagent/`, `web/`, `go.work*` trees the images
build from).

### B1. GitHub App callback

In your GitHub App (the Phase 1 one), set the user-authorization callback URL to:

```
http://<APP_VM_IP>:<WEB_PORT>/api/auth/github/callback
```

This must match `PROTEOS_BASE_URL` below exactly.

### B2. Configure

```bash
cd <repo>/deploy/app-stack
cp .env.example .env
$EDITOR .env
```

Set, at minimum:
- `WEB_PORT` + `PROTEOS_BASE_URL` — the browser origin (e.g. `http://10.0.0.5:8080`).
- `PROTEOS_NODE_AGENT_URL` — `http://<KVM_HOST_IP>:9090`.
- `PROTEOS_AGENT_TOKEN` — **the same token** you set in Part A3.
- `GITHUB_APP_CLIENT_ID` / `GITHUB_APP_CLIENT_SECRET` / `PROTEOS_STATE_KEY` (`openssl rand -hex 32`).
- Keep `PROTEOS_KERNEL_REF` / `PROTEOS_ROOTFS_REF` matching the staged filenames.

**Machine templates (optional).** With nothing extra set, the control plane offers
a single "base" machine built from `PROTEOS_ROOTFS_REF`. To offer the full
catalog (Go / Node / Python / full), the node_agent Ansible role on the KVM host
bakes one image per template and fetches a `proteos-templates.json` to
`deploy/ansible/artifacts/` on the controller. Install it on the app VM:

```bash
cp <controller>/deploy/ansible/artifacts/proteos-templates.json \
   <repo>/deploy/app-stack/proteos-templates.json   # overwrites the placeholder
# in .env:
#   PROTEOS_TEMPLATES_FILE=/etc/proteos/templates.json
#   (optionally raise PROTEOS_MAX_VCPUS / _MEM_MIB / _DISK_MIB above the defaults)
```

The file is mounted read-only at `/etc/proteos/templates.json`. Each template's
default resources must fit within the `PROTEOS_MAX_*` caps or the control plane
refuses to start (an empty/placeholder catalog fails loudly with
`template catalog is empty` — by design, never a machine booting a bogus image).

### B3. Build + run

```bash
docker compose up -d --build
docker compose logs -f controlplane   # "applying migrations" then listening on :8080
```

The control plane applies migration `000002` on startup (`-migrate`), upserts the
host row from `PROTEOS_HOST_NAME` + `PROTEOS_NODE_AGENT_URL`, and starts the
poller. Confirm it reached the agent:

```bash
docker compose exec controlplane /usr/local/bin/controlplane --help >/dev/null 2>&1 || true
# from the app VM:
curl -s http://<KVM_HOST_IP>:9090/healthz   # reachable across the LAN?
```

### B4. Sign in

Open `PROTEOS_BASE_URL` in a browser → "Sign in with GitHub" → you land on the
Dashboard with a machine card (no machine yet).

---

## Part C — Phase 2 acceptance walkthrough

Tick each row of the Phase 2 checklist in `plans/proteos-poc-to-prod.md`.

1. **Create → provisioning → running (real FC VM).** Click **Create**. The card
   goes `provisioning → running` live over SSE (no refresh). On the KVM host:
   ```bash
   ps -ef | grep -i firecracker        # VMM running under a per-VM uid (>=100000)
   ls /srv/jailer/firecracker/         # one jail per machine
   sudo ip link | grep tap             # a tap device exists
   ```

2. **Every VMM is jailed.** The firecracker process runs as the dropped uid in a
   chroot — confirm the uid in `ps` is in the configured jail range, not 0.

3. **stop / start transitions persisted + one event row each.** Use the card's
   Stop then Start. Then:
   ```bash
   docker compose exec postgres psql -U proteos -d proteos \
     -c "select from_state, to_state, type, actor from machine_events order by id;"
   ```
   Expect rows for each transition (`requested→provisioning→running→stopping→stopped→starting→running`).

4. **tap + private IP; vm_handle/host recorded.**
   ```bash
   docker compose exec postgres psql -U proteos -d proteos \
     -c "select state, host_id, vm_handle, guest_ip from machines;"
   ```
   `guest_ip` populated, `vm_handle` set, `host_id` points at the seeded host.

5. **Dashboard live state + event stream.** Open the browser devtools Network
   tab → `GET /api/machine/events` stays open (SSE), and a second tab's
   Stop/Start updates the first without refresh.

6. **Basic default-deny egress (the decisive check).** Get a shell in a guest
   (via its tap/`guest_ip`, the spike's `03-network.sh` scheme), then:
   ```bash
   # inside the guest:
   curl --max-time 3 http://<gateway .1>:9090/healthz   # node-agent  -> MUST fail/time out
   curl --max-time 3 http://<KVM_HOST_IP>:9090/healthz  # agent (host)-> MUST fail
   curl --max-time 3 http://<APP_VM_IP>:8080/           # control plane -> MUST fail
   curl --max-time 3 https://1.1.1.1                    # internet    -> MUST succeed
   ```
   This exercises both nft hooks: the **input** chain drops guest→host-local
   services (the gateway IP, where the node-agent listens — the forward chain
   never sees host-destined traffic, so this must be blocked at `input`); the
   **forward** chain drops guest→RFC1918/link-local and NATs everything else to
   the internet.

   **If a check is wrong, diagnose with the per-rule counters** (on the KVM host,
   while re-running the guest curl):
   ```bash
   sudo nft list table ip proteos          # which rules' counters increment?
   sysctl net.ipv4.ip_forward              # must be 1 for the internet path
   sudo conntrack -L | grep <guest_ip>     # is the connection NATed (src rewritten to the host)?
   ip route get 1.1.1.1                     # the host itself must reach the internet
   sudo nft list ruleset | grep -iE 'hook forward|policy drop'   # a competing table (docker/ufw/firewalld) dropping forward?
   ```
   Reading the counters for a guest→`1.1.1.1` attempt:
   - `forward … iifname <tap> oifname <egress> accept` **and** the `postrouting …
     masquerade` counters both climb, but no reply → return path / an upstream
     firewall is dropping the masqueraded traffic.
   - the `forward … accept` counter does **not** climb → packets aren't being
     forwarded: `ip_forward=0`, a competing `forward` chain with `policy drop`
     (Docker/ufw/firewalld on the KVM host), or a routing problem.
   - `masquerade` doesn't climb but `forward accept` does → NAT not applied
     (egress interface mismatch); confirm `egress` matches `ip route get 1.1.1.1`.

   > After deploying a node-agent fix, **existing machines keep their old rules**
   > (rules are written at boot). Destroy/recreate the machine, or stop+start it,
   > to pick up the new ruleset.

7. **Authenticated agent channel.** Temporarily blank/!wrong-token a control-plane
   restart → agent calls 401 and machines go to `error` with reason; restore the
   token to recover.

8. **Driver interface + pinned kernel/rootfs on the machine record.**
   ```bash
   docker compose exec postgres psql -U proteos -d proteos \
     -c "select kernel_ref, rootfs_ref, resource_spec from machines;"
   ```

---

## Part D — OpenBao secrets (Phase 5)

The app stack ships an `openbao` service (file storage, persistent `baodata`
volume). It is the production secrets backend; the dev `file` backend stays the
default so a fresh stack serves logins immediately. OpenBao boots **sealed** and
re-seals on every restart — manual unseal is an accepted Phase 5–11 cost (HA +
auto-unseal are Phase 12).

### D1. One-time init

On the app VM, with the `bao` CLI and `jq` installed (`BAO_ADDR` = the published
openbao port):

```bash
cd deploy/app-stack
touch openbao-secret-id                       # the controlplane bind-mounts it
docker compose up -d openbao                   # bring OpenBao up (it boots sealed)
export BAO_ADDR=http://127.0.0.1:8200
./openbao-init.sh
```

`openbao-init.sh` is idempotent. It: inits (1 key / threshold 1, saved to
`openbao-init.json`), unseals, logs in, enables KV v2 at `secret/`, a file audit
device (`/openbao/logs/audit.log` on the `baologs` volume), and AppRole; writes
policy `cp-base`, the `proteos-user` token role, and the `proteos-cp` AppRole;
then emits `PROTEOS_OPENBAO_ROLE_ID` into `.env` and the AppRole secret_id into
`./openbao-secret-id`.

> **Keep `openbao-init.json` and `openbao-secret-id` safe and uncommitted** (both
> are gitignored). `openbao-init.json` holds the unseal key + root token.

### D2. Switch the control plane to OpenBao

```bash
# in deploy/app-stack/.env
PROTEOS_SECRETS_BACKEND=openbao
# (PROTEOS_OPENBAO_ROLE_ID was filled in by the init script)

docker compose up -d controlplane
docker compose logs controlplane | grep 'secrets backend'   # → backend=openbao
```

### D3. Migrate an existing dev FileStore (optional)

If you ran on the `file` backend and have `secrets.json` on the `cpdata` volume,
copy it into OpenBao once:

```bash
docker compose exec controlplane \
  /usr/local/bin/controlplane -migrate-secrets /var/lib/proteos/secrets.json
```

### D4. After a restart — unseal

`docker compose restart openbao` (or a host reboot) leaves OpenBao sealed; the
control plane's secret reads fail until you unseal:

```bash
cd deploy/app-stack
BAO_ADDR=http://127.0.0.1:8200 \
  bao operator unseal "$(jq -r '.unseal_keys_b64[0]' openbao-init.json)"
```

### D5. Verify + backup

```bash
# A key written via the UI lands under the user's path (operator view):
BAO_TOKEN=$(jq -r .root_token openbao-init.json) \
  bao kv get secret/users/<user-uuid>/providers/claude

# Audit log is being written:
docker compose exec openbao cat /openbao/logs/audit.log | tail
```

Back up the `baodata` volume (e.g. `docker run --rm -v proteos_baodata:/d -v
"$PWD":/b alpine tar czf /b/baodata.tgz -C /d .`) alongside `openbao-init.json`;
restoring one without the other is useless.

---

## Part E — Phase 5 acceptance walkthrough (secrets + Claude Code)

Run this on the live Proxmox stack after Parts A–D. Tick each row of the
master-plan Phase 5 checklist in `plans/proteos-poc-to-prod.md` as you go.

### E0. Prerequisites

- OpenBao deployed + initialized + unsealed, `PROTEOS_SECRETS_BACKEND=openbao`
  (Part D), control-plane log shows `secrets backend backend=openbao`.
- A rootfs baked with Claude Code (`image/build-rootfs.sh --claude-bootstrap`,
  or `--claude-binary …` for an air-gapped pin; task 5.5), copied into
  `PROTEOS_AGENT_IMAGES_DIR`, and `PROTEOS_ROOTFS_REF` re-pinned to it on both the
  control plane and the node-agent. Confirm the pin is real (not the placeholder):
  ```bash
  grep -E 'image|claude_version|claude_sha256' image/manifest.lock
  # → NOT "(not yet built)" / "(none)". If it is, re-bake on the host and commit
  #   the real manifest.lock before going further.
  ```
- **DNS works inside the guest.** The guest gets a static IP from the kernel `ip=`
  cmdline, which sets no resolver — `image/build-rootfs.sh` bakes a static
  `/etc/resolv.conf`. Confirm on the guest (an **Open terminal**) before trusting
  anything below, because a missing resolver looks like a Claude auth failure
  (`FailedToOpenSocket`), not a DNS error:
  ```bash
  cat /etc/resolv.conf              # nameserver present, NOT a dangling symlink
  getent hosts api.anthropic.com    # resolves
  curl -sI https://api.anthropic.com | head -n1   # HTTP/2 … (egress NAT + DNS both up)
  ```
- Have a **real Anthropic API key** ready (`sk-ant-…`).

### E1. Set the key (never echoed)

In the browser, open the dashboard → **AI providers** → Claude Code → **Set key**,
paste the key, Save. The badge flips to **Key set**. Verify it never leaves:

```bash
# In OpenBao, under the USER's path (operator view). <uuid> = the user's id.
BAO_TOKEN=$(jq -r .root_token deploy/app-stack/openbao-init.json) \
  bao kv get secret/users/<uuid>/providers/claude        # → api_key present

# NOT in Postgres:
docker compose -f deploy/app-stack/compose.yaml exec postgres \
  pg_dump -U proteos proteos | grep -c 'sk-ant'          # → 0

# NOT in the logs (control plane + node-agent):
docker compose -f deploy/app-stack/compose.yaml logs controlplane | grep -c 'sk-ant'   # → 0
journalctl -u proteos-node-agent | grep -c 'sk-ant'      # → 0 (on the KVM host)
```

### E1b. Per-user policy denial (proven in OpenBao)

The per-user restriction must hold **in Bao**, not just in our Go — a confused-deputy
bug that builds the wrong path has to fail at the storage layer. 5.0's
`bao_test.go` proves this against a testcontainer; this is the live spot-check.
Mint a token scoped to a *different* user and try to read this user's path — it
must be denied:

```bash
ROOT=$(jq -r .root_token deploy/app-stack/openbao-init.json)
OTHER=11111111-1111-1111-1111-111111111111            # any uid != the real <uuid>
TOK_B=$(BAO_TOKEN=$ROOT bao token create -policy="user-$OTHER" \
  -ttl=90s -orphan -field=token)
VAULT_TOKEN=$TOK_B bao kv get secret/users/<uuid>/providers/claude
#   → 403 permission denied  (user B physically cannot read user A's secret)
```

### E2. Launch Claude Code → write a file

With the machine **running**, click **Launch Claude Code** on the machine card.
A terminal opens running `claude` (authenticated by the injected key). Prompt it:

> Create a file `~/workspace/hello-proteos.txt` containing "it works".

Then confirm on the guest (serial console or a plain **Open terminal**):

```bash
cat ~/workspace/hello-proteos.txt        # → it works
```

> **First-run approval (drift-prone — verify here).** If `claude` shows a "Do you
> want to use this API key?" prompt on first launch, accept it once. It persists
> in `~/.claude*` on the bind-mounted persistent home, so it does not recur across
> stop/start. If it recurs, finalize the pre-answer mechanism (managed settings /
> `~/.claude.json`) and fold it back into `image/claude-managed-settings.json`.

### E3. Injection on start AND resume

Injection fires on every `* → running` transition (poller hook) and again,
idempotently, before each launch. Phase 4 (hibernate/resume) has landed, so both
legs are testable now — tick "and resume" for real, no longer deferred.

```bash
# Cold path: Stop, then Start. After it reaches running, on the guest:
ls -l /run/proteos/env/claude.env        # 0600, present again
# Resume path (Phase 4 hibernate/resume): Stop hibernates; Start resumes.
# Re-run the check above after the resume reaches running — the env file must
# reappear (re-pushed on resume, so a stale snapshot secret is refreshed).
```

### E4. Audit rows (put / read / launch)

```bash
docker compose -f deploy/app-stack/compose.yaml exec postgres psql -U proteos -d proteos \
  -c "select actor, action, target from audit_log order by id desc limit 10;"
# Expect: secret.put (actor user:<uuid>), secret.read (actor system:injector,
# target = the path — never the value), agent.launch (actor user:<uuid>).
# And the openbao audit device:
docker compose -f deploy/app-stack/compose.yaml exec openbao tail /openbao/logs/audit.log
```

### E5. Reload mid-session reattaches

With a Claude session running, reload the browser tab. The agent terminal
reattaches to the same `agent-claude` session with scrollback intact (the session
outlives the WebSocket — Phase 3 property reused for agents).

### E6. Negative paths

- Remove the key (**Remove** in the panel) → the **Launch Claude Code** button is
  replaced by the "set a key" CTA; opening `/gw/agent/claude` directly would 409.
- Stop the machine → the launch button disappears; the agent route would 409
  `machine_not_running`.

When every row passes, tick the master-plan Phase 5 checklist in
`plans/proteos-poc-to-prod.md`.

## Part F — Phase 6 acceptance walkthrough (Gemini, Codex, pi.dev)

Phase 6 adds three providers as **data + rootfs**, no control-plane code. The
extra CLIs are **opt-in**: you bake the ones you want and tell the control plane
which it may offer. Run this after Part E on the live stack.

### F0. Bake the providers you want into the image

Providers install the **latest** version by default (pin with `proteos_<p>_version`
in Ansible / `--<name>-version` on the script). The bake records the resolved
versions in `manifest.lock`. Via Ansible (preferred — see
`deploy/ansible/README.md`):

```bash
ansible-playbook -i inventory.ini site.yml \
  --extra-vars "proteos_agent_token=$(openssl rand -hex 32) \
                proteos_gemini_install=latest proteos_codex_install=latest proteos_pi_install=latest"
```

or directly: `image/build-rootfs.sh --base <ci.ext4> --claude-bootstrap --gemini --codex --pi`.

- The image grows **materially** over the claude-only Phase 5 image (Node alone
  is ~120 MiB unpacked); the script reserves +512 MiB headroom when any provider
  CLI is baked. Capture the final size + build time with `image/bake-report.sh`
  (writes them into `image/PROVIDERS.md`).
- Ansible re-pins `PROTEOS_ROOTFS_REF` automatically; for a manual bake, copy the
  emitted `.ext4` into `PROTEOS_AGENT_IMAGES_DIR` and re-pin it. Confirm the pins:
  ```bash
  grep -E 'node_version|gemini_version|codex_version|pi_version|features' /var/lib/proteos/images/manifest.lock
  ```
- On a guest **Open terminal**, confirm the CLIs you baked are present (e.g.
  `claude --version && codex --version && pi --version`).

### F0b. Tell the control plane which providers to offer

The registry seeds all four providers, but the UI must only offer the ones
actually baked. Set `PROTEOS_PROVIDERS_ENABLED` (app-stack env) to **match your
bake flags** — CSV of provider keys; the control plane enables exactly these and
disables the rest on startup:

```bash
# matches a bake of claude + codex + pi (no gemini):
PROTEOS_PROVIDERS_ENABLED=claude,openai,pi
```

(`gemini` registry key = `--gemini` bake flag; `openai` = `--codex`. Default if
unset in compose is `claude` only.) Restart the control plane; its log shows
`provider enablement reconciled`. The dashboard now offers only the baked set.

### F1. Set keys + launch each provider

In **AI providers**, set a key for Gemini (`GEMINI_API_KEY`), OpenAI Codex
(`OPENAI_API_KEY`), and Pi (Anthropic key). Each provider renders its own form
from `secret_fields` — no per-provider UI. A **Launch <name>** button appears for
every enabled+keyed provider. Launch each and prompt it to touch a file in the
workspace.

### F2. Verify Codex's setup login ran

Codex authenticates via the registry `setup_command` (`codex login
--with-api-key`), run by the guest agent on every push. After launching Codex:

```bash
# on the guest:
test -f ~/.codex/auth.json && echo "auth.json present"
# the key must NOT appear in any log:
sudo journalctl -u proteos-guestagent | grep -i OPENAI_API_KEY   # → no matches
```

A failing setup degrades the provider: launching it closes the terminal with a
`setup_failed` message instead of a broken TUI; fixing the key and re-launching
re-runs setup and clears it.

### F3. Re-injection across stop/start + reattach

Stop/start the machine → all four providers are re-injected and re-launchable
(the poller pushes on every `→ running`). Reload the browser mid-session → the
agent session reattaches with scrollback (same as Part E5).

When every row passes, tick the master-plan Phase 6 checklist in
`plans/proteos-poc-to-prod.md`.

---

## Part G — Phase 8: per-machine editor (code-server) subdomains

The browser editor is **code-server inside the microVM**, reached only through
the authenticated gateway at a per-machine subdomain `m-<uuid>.<machine-domain>`
(master plan "Web origin isolation"). It needs wildcard DNS + TLS at the proxy
layer and one config flag on the control plane; the app-stack nginx already
carries the wildcard `server_name` block (Phase 8). The editor binary is baked
into the rootfs by the node_agent Ansible role (`proteos_codeserver_install`,
default on).

### G1. Wildcard DNS + TLS (NPMplus / proxy layer)

Pick a parent domain for editors (separate label from the app, e.g.
`machines.proteos.example`). At the proxy that already fronts the app VM
(NPMplus in this lab):

```
# 1. DNS: a WILDCARD A/AAAA record for the editor parent domain → app VM IP.
#    *.machines.proteos.example   A   <APP_VM_IP>
#    (the app host itself, e.g. proteos.example, keeps its own record.)

# 2. TLS: a WILDCARD certificate for *.machines.proteos.example. Per-host certs
#    cannot cover unbounded m-<uuid> names, so this MUST be a wildcard — issue it
#    via a DNS-01 challenge (NPMplus → SSL → Let's Encrypt → DNS challenge).

# 3. Proxy host in NPMplus: forward *.machines.proteos.example → app VM :<WEB_PORT>
#    with WebSockets enabled (the editor is WS-heavy). The app-stack nginx then
#    host-routes m-<uuid>.… to the control plane (its wildcard server block).
```

### G2. Turn it on (app stack)

```
# in deploy/app-stack/.env
PROTEOS_MACHINE_DOMAIN=machines.proteos.example
# Cookies are cross-site (SameSite=None; Partitioned) — they REQUIRE Secure, so
# the editor only works over HTTPS. Keep PROTEOS_COOKIE_SECURE=true behind TLS.
docker compose up -d controlplane web
```

Leaving `PROTEOS_MACHINE_DOMAIN` unset disables the editor entirely (the
dashboard's "Open editor" mints but the route 404s) — no wildcard infra needed
for a non-editor deployment.

### G3. Bake check + live tunnel (KVM host)

```
# Non-destructive image check (loop-mount): code-server present + web-forward
# wiring + no auth baked.
sudo image/verify-phase8-rootfs.sh

# Live: boot a microVM and prove the editor tunnel (port 1025 → code-server).
# Run on a FRESH node (or stop the node-agent first).
sudo image/verify-phase8-live.sh
```

### G4. Live acceptance (app stack, browser)

Walk the master-plan Phase 8 checklist:

1. Dashboard → **Open editor** on a running machine → the editor iframe loads
   code-server (opens `/workspace`). Edit + **save** a file.
2. Open a **terminal** on the same machine → the saved file is visible (and a
   file written in the terminal appears in the editor) — shared `/workspace`.
   *(Durable across stop/start once Phase 4's disk has landed.)*
3. **No direct route to the guest**: from outside the host, a port scan of the
   editor subdomain shows only the proxy ports; hitting `m-<uuid>.<domain>/`
   **without** the editor cookie returns 401.
4. **Logout in another tab** kills the editor: the live code-server socket drops
   and a reload returns 403 (the parent-session-alive check).
5. **DevTools** on the editor subdomain: the `proteos_machine` cookie is present
   with `Secure; HttpOnly; SameSite=None; Partitioned`, and the main
   `proteos_session` cookie is **absent** (it is host-only — fact #1).

Tick the boxes in `plans/proteos-poc-to-prod.md` (the "edits persist" box notes
the Phase 4 gating if the disk has not landed).

### G5. Gotchas

- **Wildcard `server_name` block.** Without the Phase 8 nginx block, every
  `m-<uuid>.…` request matches the catch-all `server_name _` and returns the
  SPA's `index.html` with **200** — the exact silent-failure class the `/gw/`
  WebSocket gotcha already bit us with. The block is in
  `deploy/app-stack/nginx.conf`; if editors return the dashboard HTML, that block
  (or the NPMplus WebSocket toggle) is missing.
- **`X-Forwarded-Proto` at the proxy.** TLS terminates at NPMplus, which proxies
  to the app-stack nginx over HTTP; the app nginx forwards the *inbound*
  `X-Forwarded-Proto` (not `$scheme`), so the control plane mints `https://`
  editor URLs. If "Open editor" yields a `http://…` URL (mixed-content blocked),
  that header passthrough is wrong.
- **`X-Frame-Options` on the editor host.** The editor is embedded cross-origin
  in the dashboard; framing is governed by the control plane's
  `Content-Security-Policy: frame-ancestors <dashboard origin>`. A stray
  `X-Frame-Options: SAMEORIGIN` (NPMplus adds one per proxy host by default, and
  the control plane strips any that code-server emits) **overrides** the CSP and
  blocks the iframe ("Refused to display … in a frame"). Set **X-Frame-Options →
  none** on the `*.<machine-domain>` NPMplus proxy host. Also enable that host's
  **Websockets Support** (code-server's terminal/extensions need it).
- **Editor 401 / cookie dropped.** The `proteos_machine` cookie is
  `SameSite=None; Partitioned`, which browsers only accept when also `Secure` —
  so the editor only works over HTTPS (it is, behind NPMplus). The control plane
  always marks this cookie `Secure`; if a request to `m-<uuid>.…/` 401s right
  after the auth redirect, check DevTools → the auth response's `Set-Cookie` and
  whether `/` carries the cookie back (a browser with third-party cookies hard-
  blocked and no CHIPS support falls back to the "Open in new tab" button).

---

## Reproducibility notes

- **Pinned versions** live in `spike/firecracker/env.sh` + the committed
  `versions.lock`; firecracker/jailer/kernel/rootfs come from there.
- **A fresh person needs**: (1) a Proxmox VM per `00-proxmox-vm.md` + the spike's
  `01-host-setup.sh`; (2) any Docker host for the app stack; (3) the two
  `.env.example` files filled in with a shared token and matching image refs.
- **No host-specific paths are baked into code** — they are all env vars with
  documented defaults.
- **Clean slate** before a run: `spike/firecracker/07-teardown.sh` clears stale
  taps/nft rules; `docker compose down -v` drops the app stack + DB.

## Gotchas

- The agent's `PROTEOS_AGENT_SUBNET` (`172.30.0.0/24`) must not collide with the
  Proxmox bridge or the app VM subnet.
- `PROTEOS_BASE_URL`, `WEB_PORT`, and the GitHub callback URL must all agree, or
  OAuth fails with a redirect-uri mismatch.
- Containers can't reach a `127.0.0.1` agent — use the KVM host's LAN IP (split
  VMs) or `host.docker.internal` (same host).
- On plain HTTP keep `PROTEOS_COOKIE_SECURE=false`; put TLS in front for real use.
- **Phase 8 editor** needs a wildcard DNS record + wildcard TLS for
  `*.<PROTEOS_MACHINE_DOMAIN>` and `PROTEOS_COOKIE_SECURE=true` (the editor cookie
  is `SameSite=None; Partitioned`, which browsers only send with `Secure`). If the
  editor shows the dashboard HTML instead, the nginx wildcard `server_name` block
  or the proxy's WebSocket toggle is missing (see Part G5).
- **Default-deny system FORWARD policy** (Docker, ufw, or a manual `iptables -P
  FORWARD DROP` on the KVM host): the guest gets no internet even though the
  `proteos` forward rules accept it, because that drop lives in a separate
  iptables-managed `ip filter` chain our table can't override. The driver detects
  the `ip filter` FORWARD chain and adds tap accept rules there. Caveat: a `ufw
  reload` / `docker` restart can flush those — they're reapplied on the next
  machine boot. Diagnose with `sudo nft list ruleset | grep -iE 'hook forward'`
  (look for `policy drop`).
