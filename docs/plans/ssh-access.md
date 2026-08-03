# Plan: SSH access into microVMs

> Status: **not started** (planning document only — no code changes).
> Scope: two things users asked for — (a) an SSH server running inside every
> guest microVM, and (b) a way for users to define SSH *login* keys that get
> added to (and removed from) their machines automatically. Distinguish this
> sharply from work that already exists under the same "SSH" word: the
> **portable-profile SSH key** (`plans/portable-user-profile-implementation.md`
> Phase 4) is an **outbound** key the guest uses to `git clone`/`push` over SSH
> to GitHub/GitLab/etc. It has no relationship to logging *into* a machine.
> This plan is entirely about **inbound** access.

## 1. Overview / goal

Today the only way to get a shell in a ProteOS microVM is the browser-based
xterm.js terminal, reached over a WebSocket that the control-plane gateway
proxies through the node-agent's vsock tunnel to the guest agent
(`controlplane/internal/gateway/terminal.go`). There is no way to:

- run a native `ssh user@machine` from a local terminal,
- use `scp`/`sftp`/`rsync -e ssh`,
- point an IDE's "Remote-SSH" feature at a machine,
- or otherwise use any standard SSH-based tooling against a machine.

Goal: let a user register one or more SSH **public** keys against their
ProteOS account, have those keys automatically authorize login on every
machine they create (and on already-running machines when keys change), run
an SSH server inside the guest, and reach that server through a path that
preserves ProteOS's existing security model (nothing in a guest is reachable
except through the authenticated control plane — see §3.4).

## 2. Current state: what exists today and what's missing

This section is the result of reading the actual code, not the older plan
docs' descriptions of intent. File:line references are current as of this
writing.

### 2.1 What already exists (reusable)

**A different SSH feature already shipped — outbound git SSH, not inbound login.**
It is real, working, and worth understanding precisely so it isn't confused
with (or accidentally reused incorrectly for) this plan:

- `controlplane/internal/profile/sshkey.go` — `GenerateSSHKey()` creates a
  server-side ed25519 keypair (unencrypted OpenSSH PEM private key +
  `authorized_keys`-format public key + SHA256 fingerprint). Note:
  "`authorized_keys`-format" here is only a *text encoding* of the public key
  (the same format GitHub's "Add SSH key" box expects) — nothing writes an
  actual `authorized_keys` file into a guest anywhere in the repo today.
- `controlplane/internal/profile/store.go:307-350` (`SetSSHKey`,
  `SSHPublicKey`, `DeleteSSHKey`) and `profile.go:122-151` — this key is
  stored in OpenBao, materialized into the guest at `~/.ssh/id_ed25519`
  (mode 0600) plus a static `~/.ssh/config` (mode 0600) scoped to
  `github.com`/`gitlab.com`/`bitbucket.org`/`ssh.dev.azure.com` with
  `IdentityFile ~/.ssh/id_ed25519`. It is the guest's *outbound* SSH client
  identity for talking to git hosts.
- `controlplane/internal/httpapi/profile_git_ssh.go:141-206` — `GET/POST/DELETE
  /api/profile/ssh`. POST generates-or-replaces (no import of a
  user-supplied key, no multiple keys, no expiry).
- `web/src/components/GitSshPanel.tsx` — the Settings UI for this feature
  ("Git & SSH" panel).

**The delivery/propagation plumbing this git-SSH feature rides on is exactly
what §3 below reuses for login keys** — it is the single most important
piece of existing infrastructure for this plan:

- `controlplane/internal/injector/injector.go` — `Injector.Inject(ctx,
  userID, machineID)` composes a `guestwire.SecretsRequest` from a user's
  profile items (provider env vars + arbitrary `FileDef`s materialized under
  `$HOME`) and pushes it over the vsock tunnel to the guest agent's `PUT
  /secrets` (`injector.go:195-234`, dialing `GuestDialer.DialGuest(...,
  agentapi.GuestTerminalPort)`). It's a full replace-all push, so removing an
  item on the control-plane side clears it from the guest on next inject —
  exactly the semantics needed for key revocation.
- `injector.go:73-94` (`InjectAsync`) and
  `controlplane/internal/httpapi/profile.go:178-195`
  (`reinjectRunningMachines`) — any profile mutation triggers a best-effort,
  bounded-retry re-push to every currently-*running* machine the user owns.
  New machines pick up current state automatically as part of normal
  injection (fired "on every `* → running` transition" per
  `injector.go:8-11`).
- `guestagent/api/guestwire.go:653-673` (`FileDef`, `SecretsRequest.Files`) —
  the generic "write this content to this `$HOME`-relative path with this
  mode" mechanism. Its doc comment already anticipates SSH key material:
  *"Content is the file body and is SENSITIVE (e.g. a private SSH key)"*.
  This is guest-agnostic; it already correctly `chown`s newly created parent
  directories (`guestagent/internal/secrets/secrets.go:412-413`, comment:
  *"a freshly created ~/.ssh is owned by the user (root-owned dirs in a
  user's home break tools like ssh)"* — i.e. this exact scenario, an
  `authorized_keys` file under `~/.ssh`, was anticipated even though nothing
  writes one today).
- `controlplane/migrations/000011_profile_items.up.sql` /
  `000012_profile_item_mode.up.sql` — the `profile_items(user_id, key, kind,
  target, mode, expires_at, ...)` table. **Important limitation**: primary
  key is `(user_id, key)` — one row per named item. This works for a single
  git-identity SSH key but does **not** support multiple named login keys per
  user (see §3.2 — a new table is needed).

**The raw-byte tunnel and per-port forwarding pattern already exists** and is
the direct architectural analog for reaching an in-guest SSH daemon:

- `nodeagent/api/agentapi.go:36-110` — `RouteGuest` opens an opaque
  bidirectional byte tunnel from the control plane, through the node-agent,
  to a chosen guest vsock port. Three ports are defined today:
  `GuestTerminalPort=1024`, `GuestWebPort=1025` (code-server, dialed
  directly), `GuestPreviewPort=1026` (a **generic forwarder** — the
  node-agent writes a `"<port>\n"` preamble naming an arbitrary in-VM
  loopback port, and the guest bridges to it).
- `guestagent/internal/previewfwd/previewfwd.go` — the guest-side
  implementation of that generic forwarder: accept → read port preamble →
  dial `127.0.0.1:<port>` → raw-copy bytes both ways
  (`previewfwd.go:65-98,114-122`). **This is byte-for-byte the shape an SSH
  forwarder needs** (with the target port fixed at 22 instead of
  client-chosen), and could plausibly even be the same forwarder reused
  (§3.4 discusses both options).
- `controlplane/internal/gateway/terminal.go` /
  `controlplane/internal/gateway/guestdial.go` — the gateway-side pattern for
  authenticating a browser WebSocket (`Origin` allowlist, session cookie via
  the existing `requireAuth` middleware, `Registry`-based revocation on
  logout closing live connections with a close code), then dialing the guest
  tunnel and relaying bytes. `plans/port-preview-implementation.md` shows the
  same pattern extended to arbitrary HTTP/WS backends via a mint-token +
  subdomain-scoped-cookie exchange for browser contexts.
- Network policy: `nodeagent/internal/driver/firecracker/network.go` sets up
  a per-VM tap + NAT with a **default-deny nftables policy**
  (`network.go:73-122,282-317`) — no inbound path to a guest from outside the
  host exists or is intended to exist; guests only get outbound egress
  (subject to `NetworkPolicy` mode) and the vsock tunnel. This is a
  deliberate, repo-wide security invariant (see `plans/port-preview-implementation.md`
  §"Scope": *"Private only — owner-authenticated... Public sharing is an
  explicit non-goal"*) that this plan must not violate.
- No boot-time injection mechanism exists (no cloud-init, no MMDS, no config
  drive — confirmed by reading `firecracker.go`'s `coldBoot`/`bootArgs`).
  Every guest boots from an identical pinned rootfs image
  (`firecracker.go:344`); all per-user customization happens post-boot over
  the vsock tunnel. Any SSH design must follow this pattern (push
  `authorized_keys` after boot, not bake per-user keys into the image).

### 2.2 What's completely missing

Confirmed by reading `image/build-rootfs.sh` (~1250 lines, zero `ssh`
matches), `image/manifest.lock`, all four `image/verify-phase*.sh` scripts,
and all of `guestagent/`:

1. **No SSH server in the guest image at all.** `image/build-rootfs.sh` never
   installs `openssh-server` (or `dropbear`) — the only packages baked in are
   `git`/`ca-certificates`, `sudo`, `vim`, and the Python 3 toolchain. No
   `sshd_config` is written. `image/manifest.lock`'s `features` line
   (`terminal,persist,resume,providers,claude,git,user,codeserver,node,pi`)
   has no `ssh` entry, unlike code-server/node/etc. which get pinned
   version+sha256 fields.
2. **No systemd unit for sshd.** `image/` contains exactly one systemd unit,
   `proteos-guestagent.service`; there is no `sshd.service` or
   `proteos-sshd.service` equivalent.
3. **No `authorized_keys` handling anywhere.** Zero references to
   `authorized_keys` as an actual file operation in `guestagent/` or
   `controlplane/` — the two "authorized_keys" mentions that exist are both
   about the *text encoding* of a public key (§2.1), not about writing a
   login-key file.
4. **No guest-agent SSH awareness.** `guestagent/api/guestwire.go`'s full
   list of `/control` ops (`git.configure`, `git.clone`, `git.credential`,
   `git.push`, `agent.run/cancel/event/done`, `projects.list`, `kv.get/set`,
   `git.status/diff/branch/commit`, `claude.configure`) has no `ssh.*` op.
   The guest agent doesn't start, stop, or health-check any SSH daemon.
5. **No data model for multiple login keys.** `profile_items` is
   one-row-per-`(user_id, key)`; there's no table shaped for "N public keys
   per user, each addable/removable/labeled independently" (which is what
   inbound login needs — think "GitHub → Settings → SSH keys", a list, not a
   single slot).
6. **No API for login keys.** `/api/profile/ssh` is entirely the outbound
   git-key feature (§2.1) and is the wrong shape for this anyway (generate
   only, single key, no label, no way to *import* a client-held public key —
   which is the normal SSH login-key model: the private key never leaves the
   user's laptop).
7. **No network path to reach guest port 22 from outside the host at all**,
   by design (default-deny nftables `forward` chain, no DNAT rules anywhere
   in `network.go` or the ansible roles). Confirmed by grep: the only
   `SSH:22` rule in `network.go` (`network.go:63,111,148`) is an accept for
   the **host's own** sshd from trusted management interfaces
   (`MgmtIfaces`), explicitly commented "tap devices must never be listed as
   management interfaces" — i.e. it is unrelated to, and does not enable,
   reaching a guest's sshd.
8. **No CLI or web-app affordance to open an SSH session.** The `cli/`
   package (a control-plane management CLI: `machines.go`, `task.go`,
   `project.go`, etc.) has no terminal/SSH client code at all — only the
   browser xterm.js terminal exists as a shell-access surface today.

## 3. Proposed implementation

### 3.1 Guest gets an SSH server

Bake OpenSSH server into the rootfs image, matching the existing
package-install and version-pinning conventions in `image/build-rootfs.sh`
and `image/manifest.lock` (see how `codeserver_version`/`node_version` are
tracked there).

- Install `openssh-server` via the existing `apt_extract_install` machinery
  (same pattern as the `git ca-certificates` / `sudo` / `vim` installs
  already in the script).
- Ship a minimal, hardened `sshd_config` as a new file in `image/` (mirrors
  how `image/proteos-guestagent.service` is a checked-in unit file that
  `build-rootfs.sh` installs):
  - `PasswordAuthentication no`, `PermitRootLogin
    prohibit-password`/`no` (decide based on whether the guest's primary
    user is root or a `sudo` user — check current guest user model before
    finalizing; `image/build-rootfs.sh`'s `sudo` install suggests a non-root
    user exists),
  - `PubkeyAuthentication yes`, `AuthorizedKeysFile .ssh/authorized_keys`,
  - `ListenAddress 127.0.0.1` **only** — sshd must not bind the guest's tap
    interface at all; it is reached exclusively through the vsock forwarder
    (§3.4), which is the same "loopback-only, gateway is the sole
    authenticator" trust model already used for code-server
    (`plan-phase-8`: *"`--auth none` is safe because 13337 is loopback-only
    ... the gateway is the authenticator"*) and the port-preview
    backend-dial (`previewfwd.go` dials `127.0.0.1:<port>` only).
  - Disable unused auth methods (`ChallengeResponseAuthentication no`,
    `KbdInteractiveAuthentication no`, `X11Forwarding no` unless there's a
    concrete want for it), keep `AllowTcpForwarding`/`AllowAgentForwarding`
    as explicit decisions (open question, §5).
- New systemd unit `proteos-sshd.service` (or just enable the distro's
  stock `ssh.service` if the config file alone is sufficient — cheaper,
  fewer moving parts, prefer this unless a reason emerges not to), installed
  the same way `proteos-guestagent.service` is (`build-rootfs.sh:1225-1230`
  — manual `multi-user.target.wants` symlink since chroot has no running
  systemd to `systemctl enable` against).
- Add `openssh_version`/`openssh_sha256` (or equivalent apt-pin) fields to
  `image/manifest.lock` and an `ssh` entry to the `features` line, matching
  existing conventions.
- Extend `image/verify-phase*.sh` (or add a new `verify-ssh-rootfs.sh`) to
  assert: `sshd` binary present, unit enabled, config has the loopback-only
  bind and pubkey-only auth, no default/empty-password accounts.
- Host key generation: let `sshd` generate host keys on first boot as usual
  (`ssh-keygen -A`, standard distro postinst behavior) — **do not** bake a
  shared host key into the golden image (every VM would share one identity,
  defeating host-key verification and creating a fleet-wide impersonation
  risk if the image leaks). If deterministic/known host keys are wanted for
  UX reasons (avoiding "remote host identification has changed" warnines
  across machine recreates), that's a separate, smaller follow-up — flag as
  an open question (§5), not baseline scope.

### 3.2 Users define their SSH keys (data model + API)

This needs a genuinely new concept — **login keys** — distinct from the
existing single-slot outbound git SSH key (§2.1). Model it like GitHub's "SSH
and GPG keys" settings page: a list, not a slot.

**New table** (new migration, e.g. `0000XX_user_ssh_login_keys.up.sql`):

```sql
CREATE TABLE user_ssh_login_keys (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label       text NOT NULL,
    public_key  text NOT NULL,        -- authorized_keys-format, one line
    fingerprint text NOT NULL,        -- SHA256:... , derived at insert time
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz           -- soft delete, NULL = active
);
CREATE UNIQUE INDEX ON user_ssh_login_keys (user_id, fingerprint)
    WHERE revoked_at IS NULL;
```

(Confirm exact `users` table name/PK type against the current schema before
writing the migration — other tables in this repo use `pgtype.UUID`, follow
that convention.) Public keys are **not secret** (that's the point of
asymmetric login auth) — unlike the private key in `profile_items`, these can
live directly in Postgres with no OpenBao round-trip, simplifying the read
path considerably versus the existing `profile.Store`.

**New package**, e.g. `controlplane/internal/sshkeys/store.go` (separate from
`controlplane/internal/profile` — this is a different lifecycle: many rows,
no secret value, no injector `Def` involved) with `List`, `Add`, `Revoke`
(soft-delete, keep history) or `Delete` (hard), and `ActiveAuthorizedKeys(ctx,
userID) (string, error)` that renders every active key as one
`authorized_keys`-format blob (this is the function the injector composition
step in §3.3 calls).

**New API** in `controlplane/internal/httpapi/`, e.g. `sshkeys.go`:

```
GET    /api/ssh-keys           -> [{id, label, public_key, fingerprint, created_at}]
POST   /api/ssh-keys           <- {label, public_key}  (parse+validate via
                                    golang.org/x/crypto/ssh.ParseAuthorizedKey,
                                    reject unparsable/duplicate-fingerprint)
                                -> {id, label, public_key, fingerprint, created_at}
DELETE /api/ssh-keys/{id}      -> 204 / 404
```

Validate: reject empty label, reject keys that fail `ssh.ParseAuthorizedKey`,
reject duplicate fingerprints for the same user (the unique index enforces it
DB-side; return a friendly 409). Follow the existing CSRF pattern (`s.csrfHeader(...)`,
`server.go:230-231`) for POST/DELETE, and the existing audit pattern
(`audit.Recorder`) for add/revoke — this is exactly the kind of security-relevant
action (`ActionSSHKeyAdd`/`ActionSSHKeyRevoke`, new `audit.Action` values)
the existing `profile.Store` audits every OpenBao read/write for.

**Explicitly reject** private-key upload (there is no `POST` field for a
private key anywhere in this design) — a login key's private half must never
leave the user's machine; this is the standard SSH trust model and the
correct contrast with the *existing* git-SSH feature, whose private key
*is* server-held because it's used *by the server-side guest*, not by the
user's local client.

### 3.3 Keys are propagated to newly created machines

Reuse the existing injector pipeline (§2.1) rather than building a parallel
delivery mechanism — this is the biggest "already-built" win available here.

- `controlplane/internal/injector/injector.go`'s `compose()` (currently
  lines 99-190) gains one more file-producing step alongside
  `profile.FileValues`: call `sshkeys.ActiveAuthorizedKeys(ctx, userID)` and,
  if non-empty, add a `guestwire.FileDef{Path: ".ssh/authorized_keys", Mode:
  0600, Content: <rendered blob>}` to the composed `Files` list. This piggybacks
  on the exact mechanism `TestInjectorEmitsSSHKeyFiles` already exercises for
  the git-SSH key/config files — same code path, same guest-side directory
  creation/chown logic (`secrets.go:412-413`), same replace-all-on-every-inject
  semantics (so a revoked key is removed from the guest on the very next
  inject, not just "not re-added").
- No new dependency on OpenBao: `Injector` already holds a `secrets.Store`
  for the OpenBao-backed values; the new `sshkeys.Store` bypasses that
  entirely and reads straight from Postgres, since login keys aren't secret.
- **New machine at creation**: no boot-time change needed (§2.1 — there's no
  boot-time injection path and this plan shouldn't add one). The *existing*
  "inject on every `* → running` transition" hook already fires for newly
  created machines the moment they come up, and will now include
  `authorized_keys` automatically once §3.3's `compose()` change lands — no
  additional creation-flow code required.
- **Already-running machines on key add/remove**: `POST /api/ssh-keys` and
  `DELETE /api/ssh-keys/{id}` call the same `reinjectRunningMachines`-style
  helper the existing `/api/profile/ssh` handlers already call
  (`profile.go:178-195` / `profile_git_ssh.go:162-184,188-206`) — add the new
  handlers next to (or literally reuse, via a small shared helper extracted
  from `handleGenerateSSHKey`/`handleDeleteSSHKey`) that call.
- OpenSSH re-reads `authorized_keys` **on every connection attempt** — no
  `sshd` reload/restart is needed after a key changes, unlike the code-server
  supervisor-restart pattern in Phase 8. This meaningfully simplifies
  revocation: it's live the moment the file lands, with no service-management
  step and no window where a stale process is still using an old file.

### 3.4 How SSH reaches the VM (network design)

This is the one part of the feature with no close existing analog to
directly copy, because SSH is not HTTP/WebSocket (unlike terminal,
code-server, and port-preview, which are all HTTP(S)/WS payloads the gateway
can speak natively). Two designs, evaluated against the repo's established
"nothing in a guest is reachable except through the authenticated control
plane" invariant (§2.1, `network.go`'s default-deny policy;
`plans/port-preview-implementation.md`'s explicit "Private only" non-goal):

**Option A — control-plane-mediated tunnel (recommended).**
Add a new gateway route, e.g. `GET /gw/ssh?machine=<id>`, authenticated by
the existing session cookie + `requireAuth` middleware (same as
`/gw/terminal`), that upgrades to a WebSocket and raw-bridges bytes 1:1 (no
JSON framing needed, unlike the terminal's xterm.js protocol — SSH is
already a self-framing binary protocol) to the guest's sshd. On the guest
side, reuse `previewfwd`'s exact shape: either (a) literally point a preview
request at port 22 (if the injected-authorized-keys design makes sshd a
normal preview-able backend — cheap, zero new guest code, but conceptually
conflates a system service with the user-app preview feature and skips the
`IsSystemGuestPort`/IsSystemGuestPort` allowlisting distinction that
currently keeps port 22 out of any user-choosable range), or (b) — cleaner —
add a fourth system port, `GuestSSHPort = 1027`, directly wired (like
`GuestTerminalPort`/`GuestWebPort`) to a tiny guest-side forwarder that
dials `127.0.0.1:22` unconditionally (essentially `previewfwd.go` with the
preamble step deleted and the port hardcoded — could literally share the
`bridge()` helper). Prefer (b): it keeps sshd out of the user-app preview
port space and matches the existing "each system service gets its own vsock
port" convention (`agentapi.go:46-63`).

Client side: extend the `cli/` tool with a new subcommand, e.g. `proteos ssh
<machine>`, that authenticates the same way the rest of `cli/internal/client`
already does, opens the `/gw/ssh` WebSocket, and bridges it to stdio — either
by acting as the transport directly (spawn nothing, speak SSH's binary
protocol straight over the WS from a purpose-built process the user runs as
`ssh -o ProxyCommand="proteos ssh-proxy %h" user@<machine-id>`) or by running
as a `ProxyCommand` helper subprocess. This preserves every existing
security property (session auth, revocation-on-logout via the existing
`Registry` used by `terminal.go:96-101`, no new inbound host firewall rule,
no publicly reachable port) at the cost of requiring the `proteos` CLI (or a
documented `ssh -o ProxyCommand=...` one-liner) instead of a bare `ssh
user@host`.

**Option B — direct host port-forward (DNAT) to the guest, rejected as
baseline.** Add a per-VM iptables/nft DNAT rule (host port ↔ guest
`<tap-ip>:22`) so a bare `ssh user@<host>:<port>` works with no ProteOS
tooling involved. This is the "traditional" approach and is what the
`spike/firecracker/03-network.sh` spike proved was *mechanically possible*
(direct L3 guest reachability) — but the production driver deliberately does
not carry that forward (§2.1), and adding it now would be a first-of-its-kind
hole in the fail-closed `forward` chain: it exposes a new inbound path per VM
that bypasses control-plane session auth/revocation entirely (auth would be
100% down to SSH key validation with no session-liveness check, no
"logout closes this" story, and a new per-VM port-allocation/lifecycle
problem to solve on the host firewall). Keep this as an explicitly rejected
alternative unless a concrete requirement emerges that Option A's
`ProxyCommand` requirement is a blocker (e.g. "must work with unmodified
IDEs that can't run a ProxyCommand" — flagged as an open question, §5, since
most mainstream IDE remote-SSH features **do** support `ProxyCommand`/
`ProxyJump`, e.g. VS Code Remote-SSH).

### 3.5 Ongoing key lifecycle

Already covered by the design above; summarized for clarity:

- **Add**: `POST /api/ssh-keys` → Postgres row → `reinjectRunningMachines` →
  live on every running machine within the existing bounded-retry window; on
  any machine created afterward, picked up automatically by the standard
  boot→running injection.
- **Revoke/remove**: `DELETE /api/ssh-keys/{id}` → soft or hard delete →
  `reinjectRunningMachines` re-renders `authorized_keys` **without** the
  removed key → pushed as a full-file replace (not an append/remove diff,
  matching the injector's existing replace-all philosophy) → sshd picks it
  up on the attacker's/former-key's very next connection attempt with no
  reload needed (§3.3).
- **List**: `GET /api/ssh-keys` — metadata only (label, public key,
  fingerprint, created_at), never anything secret (there's nothing secret to
  leak — the whole point of pubkey auth).
- **Multiple machines**: because `ActiveAuthorizedKeys` is computed per-user
  (not per-machine) and the injector already fans out to every machine a
  user owns, one key list "just works" across a user's whole fleet with no
  extra plumbing — consistent with how the git-identity/git-SSH profile
  items already behave across machines today.

## 4. Files/components to touch — step-by-step

1. **`image/` — bake sshd into the rootfs.**
   - `image/build-rootfs.sh`: add `openssh-server` to the apt install list;
     write/copy a hardened `sshd_config` (loopback-only bind, pubkey-only);
     install+enable the unit (new `proteos-sshd.service` or the stock
     `ssh.service`).
   - New checked-in config file(s) in `image/` (`sshd_config`, and a unit
     file if not reusing the distro default).
   - `image/manifest.lock`: add version/sha256/feature tracking for openssh.
   - `image/verify-phase*.sh` or a new `image/verify-ssh-rootfs.sh`: assert
     sshd installed+enabled+loopback-bound+pubkey-only.
   - *Size: small–medium.* Mechanical once the exact hardening flags and
     unit choice (custom vs stock `ssh.service`) are decided.

2. **New migration — `user_ssh_login_keys` table.**
   `controlplane/migrations/0000XX_user_ssh_login_keys.{up,down}.sql`, per
   §3.2's schema (confirm FK target/type against current `users` table
   first). *Size: small.*

3. **New package — `controlplane/internal/sshkeys/`.**
   `store.go`: `List`, `Add` (parse+fingerprint+insert), `Revoke`/`Delete`,
   `ActiveAuthorizedKeys`. Unit tests mirroring `injector_test.go`'s style.
   *Size: small–medium.*

4. **New HTTP handlers — `controlplane/internal/httpapi/sshkeys.go`.**
   `GET/POST /api/ssh-keys`, `DELETE /api/ssh-keys/{id}`; wire into
   `server.go`'s route table next to the existing `/api/profile/*` block;
   reuse the CSRF/audit patterns already established there. *Size: small.*

5. **Injector change — `controlplane/internal/injector/injector.go`.**
   `compose()` gains the `ActiveAuthorizedKeys` call → `FileDef` for
   `.ssh/authorized_keys`; `Injector` struct gains an `sshkeys.Store`
   dependency (nil-safe like the existing `profile *profile.Store` field).
   Extend `reinjectRunningMachines` (or the handlers that call it) to also
   fire on `/api/ssh-keys` mutations. *Size: small — this is almost entirely
   composition of existing pieces.*

6. **New guest vsock port + forwarder.**
   `nodeagent/api/agentapi.go`: add `GuestSSHPort = 1027` alongside the
   existing three; update `IsSystemGuestPort`. Guest side: new tiny
   forwarder (or a `previewfwd` variant with the port hardcoded to 22, no
   preamble) wired into `guestagent`'s startup alongside the existing
   terminal/web/preview listeners, listening on the guest's vsock 1027 (dev:
   an extra unix socket, matching the existing dev-mode pattern). *Size:
   small — mostly copying `previewfwd.go`'s shape.*

7. **New gateway route — `controlplane/internal/gateway/ssh.go`
   (or extend the existing `gateway` package).**
   `GET /gw/ssh?machine=<id>`: auth/origin check → dial guest tunnel at
   `GuestSSHPort` → raw-bridge bytes to the upgraded WebSocket (simpler than
   `terminal.go`'s relay since there's no JSON handshake to speak to the
   guest — SSH negotiates itself). Register for revocation in the existing
   `Registry`. *Size: medium — new code, but closely modeled on
   `terminal.go`.*

8. **CLI client — `cli/internal/app/ssh.go` (or similar) + a small
   ProxyCommand-capable helper.**
   New `proteos ssh <machine>` (or `proteos ssh-proxy <machine>` for use as
   `ssh -o ProxyCommand`) that authenticates via the existing `cli/internal/client`
   session handling and bridges the `/gw/ssh` WebSocket to stdio. *Size:
   medium — new surface, but the auth/session code is entirely reused.*

9. **Web UI — new "SSH keys" panel** (e.g. alongside `GitSshPanel.tsx` in
   Settings, or a new `SshKeysPanel.tsx`): list keys with label/fingerprint,
   add-key form (paste a public key, no generation — the private key never
   leaves the user's machine, unlike the existing git-SSH panel's
   "Generate"/"Regenerate" flow), remove button per key. New hooks in
   `web/src/api/hooks.ts`/`client.ts` mirroring `useSSHKey`/`useSSHKeyMutations`
   but for the list-shaped `/api/ssh-keys` resource. *Size: small–medium.*

10. **Docs**: update `docs/architecture.md` with the new guest vsock port and
    gateway route; add an SSH-access section to whatever end-user-facing docs
    describe machine access today (check `docs/CLI.md`). *Size: small.*

Rough dependency order: (1) and (2)–(4) can proceed in parallel; (5) depends
on (3); (6) and (7) can proceed in parallel once the port number (step 6) is
picked, and (7) depends on (6) existing to dial against; (8) depends on (7);
(9) depends on (4); (1) and the rest are independent until integration
testing, which needs (1)+(5)+(6)+(7) together to prove a real login end to
end.

## 5. Edge cases and open questions

- **Which guest user does SSH log into?** The existing terminal/guest-agent
  session model needs checking: is there a single primary non-root user in
  the image (the `sudo` package's presence suggests yes), and does
  `authorized_keys` need to live under that user's home, or does the guest
  agent's existing `$HOME`-relative `FileDef` handling already resolve to
  the right home directory automatically (it should, per
  `secrets.go`'s existing `.ssh` handling for the git key — confirm the
  guest's "session user" concept lines up with sshd's login target).
- **Root login**: should `PermitRootLogin` be allowed at all, given the
  guest already grants the user root-equivalent authority via `sudo`
  (`plans/phase-8-implementation.md` explicitly frames VM isolation, not
  in-guest ACLs, as the security boundary)? Leaning "no" (login as the
  primary user, `sudo` from there) for parity with how the terminal and
  code-server already work, but worth an explicit decision.
- **Host key stability across machine recreation**: golden-image-per-boot
  (§2.1) means a machine's sshd host key is fresh every cold boot unless
  something persists it. Decide whether that's acceptable (users hit "host
  key changed" warnings on every fresh machine — mildly annoying but secure
  by default) or whether host keys should ride the Phase-4 persistent disk
  (if/when machines gain one) or be pushed by the control plane like
  `authorized_keys` is (adds complexity: now the control plane also manages
  a *private* per-machine key).
- **`AllowTcpForwarding`/`AllowAgentForwarding`/`ProxyJump` support**: useful
  for real workflows (IDE Remote-SSH typically wants agent forwarding, SFTP,
  and sometimes local/remote port forwarding) but each is additional attack
  surface inside a guest that's otherwise been deliberately kept to a narrow
  set of protocols reachable only via the gateway. Recommend enabling
  standard SFTP subsystem + agent forwarding, leaving arbitrary
  `AllowTcpForwarding` as a follow-up decision (it would let a user tunnel
  arbitrary traffic through the guest, which the existing NAT/network-policy
  work in `network.go` already governs more deliberately for the guest's own
  egress — forwarding through sshd would be a second, unaudited egress path
  and needs its own look).
- **Multiple machines, one key list**: confirmed as automatic (§3.5) — no
  per-machine key scoping in this plan. Worth confirming with product intent
  whether a user might ever want a key scoped to *one* machine only (not in
  scope here; the `user_ssh_login_keys` schema could grow a nullable
  `machine_id` scope column later without a breaking change if that's
  wanted).
- **Rate limiting / brute-force**: sshd's own defaults (`MaxAuthTries`,
  `LoginGraceTime`) plus the fact port 22 is never reachable except via an
  authenticated gateway session (Option A) means classic internet SSH
  brute-forcing isn't a threat model here — worth stating explicitly in the
  final design doc so a reviewer doesn't ask for fail2ban-style tooling that
  doesn't apply.
- **Auditing inbound SSH sessions**: should opening `/gw/ssh` be an audited
  action (like the existing `audit.Action*` events for profile/secrets)?
  Recommend yes, for parity with how sensitive the terminal/code-server
  access already… (check: is opening `/gw/terminal` itself audited today? If
  not, match that precedent rather than inventing a stricter bar just for
  SSH.)
- **Interaction with `NetworkPolicy` egress modes**: none expected — sshd
  binds loopback only and is reached inbound via vsock, not through the
  tap/NAT path `NetworkPolicy` governs. Confirm this assumption holds once
  implemented (a loopback bind should be entirely orthogonal to the guest's
  egress firewall rules, but worth a test).
- **CLI vs "just works with any ssh client" trade-off** (§3.4 Option A vs
  B): flagged as the single biggest open design question in this plan.
  Needs a product decision, not just an engineering one.

## 6. Effort estimate

Rough sizing, not a committed schedule — assumes one engineer familiar with
the codebase, sequential unless noted:

| Step | Component | Size |
|---|---|---|
| 1 | Bake openssh-server + hardened config into rootfs | S–M |
| 2 | `user_ssh_login_keys` migration | S |
| 3 | `sshkeys` store package + tests | S–M |
| 4 | `/api/ssh-keys` HTTP handlers | S |
| 5 | Injector `compose()` change (authorized_keys FileDef) | S |
| 6 | `GuestSSHPort` + guest-side forwarder | S |
| 7 | Gateway `/gw/ssh` route + relay | M |
| 8 | CLI `proteos ssh`/ProxyCommand helper | M |
| 9 | Web "SSH keys" settings panel | S–M |
| 10 | Docs | S |
| — | Integration testing (real `ssh` client end to end, revocation, multi-machine) | M |

Total: roughly **2–3 weeks** of focused work for one engineer through a
working Option-A implementation with test coverage, excluding open-question
resolution time (host-key persistence and forwarding-flags decisions in
particular could each add follow-on work if the answer is "yes, build that
too"). Steps 1–5 (server + key management + propagation, no network path
yet) are the smaller, more mechanical ~40% of the total; steps 6–8 (the
actual reachability path) are the newer, less-precedented ~50%; step 9–10
round out the rest.

## 7. Summary: what's already implemented vs what this plan builds

**Already implemented (reused as-is or with light extension):**
- Injector push/replace-all/revocation-on-delete semantics
  (`controlplane/internal/injector`).
- Guest-side generic file materialization incl. correct `~/.ssh` ownership
  (`guestagent/internal/secrets`).
- Vsock tunnel + per-port forwarder pattern, including a byte-for-byte
  analog for exactly this use case (`guestagent/internal/previewfwd`).
- Gateway auth/origin-check/revocation-registry pattern for a raw relay
  (`controlplane/internal/gateway/terminal.go`).
- CSRF/audit conventions for mutation endpoints (`httpapi/server.go`,
  `httpapi/profile_git_ssh.go`).
- Fail-closed per-VM network policy that this plan deliberately does not
  weaken (`nodeagent/internal/driver/firecracker/network.go`).

**New work this plan requires:**
- OpenSSH server baked into the guest image + hardened config + unit.
- A multi-key-per-user data model and CRUD API (`user_ssh_login_keys`,
  `sshkeys` package, `/api/ssh-keys`).
- Injector composition of `authorized_keys` from that new store.
- A dedicated guest vsock port + forwarder for sshd (or reuse of the preview
  forwarder, a smaller alternative).
- A gateway relay route for raw SSH bytes (no existing route speaks
  non-HTTP/WS payloads).
- A client-side way to actually dial that route (CLI subcommand /
  ProxyCommand helper) — the biggest genuinely new user-facing surface.
- A web UI panel for managing the login-key list.

**Explicitly not reused (despite the name overlap):** the existing
`/api/profile/ssh` single-slot, server-generated, outbound-git key feature.
It solves a different problem and its single-item `profile_items` model
doesn't fit the list-of-login-keys shape this plan needs. The two features
will coexist: a user's `~/.ssh/id_ed25519` (outbound, from the profile
feature) and `~/.ssh/authorized_keys` (inbound, from this plan) are
different files with no shared code path beyond both being `FileDef`s
pushed by the same injector.
