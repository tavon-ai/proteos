# Machine Templates + Machine Details — Implementation Plan

> Source: `plans/backlog.md` — "machine templates: go development, node dev, python,
> Go + Python + Node + build essentials…" and "machine details". Planned 2026-06-19.
> Status: **not started.**
>
> Prerequisites: Phase 6 multi-machine landed (per-user N machines, `name` column,
> switcher UI) — this builds directly on it. No hard dependency on Phases 7–9.
>
> Decisions locked with the user (2026-06-19):
> 1. **Catalog source of truth: static config in the control plane** (not a DB table,
>    not filesystem discovery). Changing the catalog = redeploy, which is fine because
>    baking an image is already an ops action.
> 2. **Resources: template defaults + user override** within caps.
> 3. **Scope: everything, including the bakes** (build-rootfs flags + producing the
>    go/node/python/full images), sequenced plumbing-first.
> 4. **Image distribution: every host carries every image** (Ansible bakes/syncs all
>    templates into each host's images dir). No template→host affinity / scheduler
>    changes in this task.

## Context

The per-machine plumbing for this feature **already exists** and is the reason this is a
modest change rather than a rewrite:

1. **Every machine already stores its own image + resources.** `machines.rootfs_ref`,
   `machines.kernel_ref`, and `machines.resource_spec` (`{vcpus,mem_mib,disk_mib}`) are
   per-row in `controlplane/migrations/000002_machines.up.sql`, surfaced in the Go model
   (`controlplane/internal/store/models.go`) and node-agent state
   (`nodeagent/internal/state/state.go`). The node-agent boots whatever ref the row
   carries: `rootfsSrc := filepath.Join(d.cfg.ImagesDir, rec.RootfsRef)`
   (`nodeagent/internal/driver/firecracker/firecracker.go:175`). **Machines can already
   run different images today** — nothing chooses a different one.

2. **Selection is the only missing piece.** `machine.Service.Create()`
   (`controlplane/internal/machine/service.go`) stamps every new machine with one global
   `RootfsRef`/`KernelRef` pinned from env (`PROTEOS_ROOTFS_REF` default `ubuntu-24.04`,
   `controlplane/internal/config/config.go`) and a global `resource_spec` from `s.spec`.
   The create API decodes only `{name}` (`controlplane/internal/httpapi/machine.go`).

3. **The bake produces one image per run.** `image/build-rootfs.sh` already has feature
   flags (`--claude-bootstrap`, Node, code-server) and is SHA-keyed →
   `proteos-rootfs-<base>-ga<sha>.ext4` with a `manifest.lock`. **Go + Taskfile bake by
   default; Python is not installed at all.** Producing focused images means new flags +
   multiple bakes + a naming scheme that distinguishes them.

4. **Machine details were never displayed** (not lost). `resource_spec`, `rootfs_ref`,
   `guest_ip`, `disk_mib`, `boot` all reach the frontend in `MachineSummary`
   (`web/src/api/client.ts`) but no component renders them. The create UI is a bare
   "+ New machine" button in `web/src/desktop/Taskbar.tsx`.

**Mental model that anchors the whole design — platform layer vs language layer:**

> A template varies the **language toolchain layer only**. The **platform layer** — guest
> agent, git, vim, Taskfile, the Claude/provider agent CLIs, code-server, the `dev` user,
> shell setup — is identical in **every** template image. The kernel is global too. So a
> "template" is, end to end: *(a chosen rootfs image baked with a chosen set of language
> layers) + (default resource spec) + (label/description)*. Everything else about a
> machine is unchanged by template choice.

## Catalog (confirmed 2026-06-19)

Language layers (toggleable in `build-rootfs.sh`): **Go**, **Node** (+ npm provider
CLIs), **Python** (python3 + pip + venv + `build-essential`). `base` carries **no**
language layer — platform baseline only — and is the catalog default (first entry).

| Template `id` | Label | Language layers | Default vcpus / mem / disk |
|---|---|---|---|
| `base`   | Base (platform only)     | none                     | 2 / 2048 / 10240 |
| `go`     | Go development           | Go                       | 2 / 2048 / 10240 |
| `node`   | Node.js development      | Node                     | 2 / 2048 / 10240 |
| `python` | Python development       | Python                   | 2 / 2048 / 10240 |
| `full`   | Full stack (Go+Node+Py)  | Go + Node + Python       | 4 / 4096 / 20480 |

Resource caps (env-configurable, applied to user overrides). **All three — vcpus, mem,
and disk — are user-overridable at create**; `disk_mib` sets the persistent LUKS disk
size and is fixed for the machine's life (no resize-later).

| Knob | Env | Min | Max |
|---|---|---|---|
| vcpus    | `PROTEOS_MAX_VCPUS`    | 1    | 8 |
| mem_mib  | `PROTEOS_MAX_MEM_MIB`  | 1024 | 16384 |
| disk_mib | `PROTEOS_MAX_DISK_MIB` | 5120 | 51200 |

**Immutable after create:** template, vcpus, mem, and disk are all fixed once the machine
exists. Changing any of them means creating a new machine (decision #5).

## Architecture decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **Catalog is a static Go config: an ordered list of `Template{ID, Label, Description, RootfsRef, KernelRef, Defaults{Vcpus,MemMiB,DiskMiB}}`.** Loaded at control-plane startup from an embedded default catalog, overridable via `PROTEOS_TEMPLATES_FILE` (JSON). `KernelRef` defaults to the global kernel when omitted. The catalog is validated on boot (non-empty, unique ids, defaults within caps); a bad catalog fails startup loudly. | The user chose static config. Embedded default keeps dev/e2e zero-config; the file override lets a deploy register the real baked `rootfs_ref`s (which carry git SHAs) without a recompile. Boot-time validation turns "image ref typo" into a startup error, not a per-machine boot failure on the node-agent. |
| 2 | **`machines` gains a nullable `template_id text` column** (migration `000007`). It is **descriptive/audit only** — the load-bearing fields stay `rootfs_ref`/`kernel_ref`/`resource_spec` already on the row. Existing machines keep `template_id = NULL` and render as "—/legacy". | The details panel must show *which* template a machine came from; `rootfs_ref` alone (`proteos-rootfs-…-ga<sha>.ext4`) is opaque. Making it nullable + non-load-bearing means zero backfill and no behaviour change for existing machines — they already boot from their stored refs. |
| 3 | **Create becomes `POST /api/machines {name?, template_id?, vcpus?, mem_mib?, disk_mib?}`.** Resolution in `Service.Create`: pick template (`template_id` or the catalog's first/default), start from its defaults, apply any provided overrides, **clamp+validate against caps** (reject out-of-range with `400 invalid_resources`; reject unknown template with `400 unknown_template`), then stamp `rootfs_ref/kernel_ref/resource_spec/template_id` onto the row. The disk is still created from the final `disk_mib`. | One code path, template-or-not, override-or-not. Keeping the global `s.spec` as the fallback default means an empty body behaves exactly as today (back-compat for the current UI and any scripts). Validation lives server-side because the cap envs and catalog live there — the UI's ranges are a convenience mirror, never the authority. |
| 4 | **New `GET /api/templates` → `[{id,label,description,defaults{vcpus,mem_mib,disk_mib}, limits{vcpus:{min,max},…}}]`.** `rootfs_ref`/`kernel_ref` are **not** exposed (internal build detail). | The create dialog needs labels, per-template defaults to pre-fill, and the cap ranges to bound the override inputs. Withholding the refs keeps build/version provenance server-side and the contract stable across rebakes (the ref's SHA changes; the template id does not). |
| 5 | **Template is fixed at create; not changeable on an existing machine.** `rootfs_ref` is immutable per machine in this task. Switching stacks = create a new machine. | The rootfs is the OS image; the persistent disk holds the user's data. Swapping the rootfs under a live disk is a separate, riskier feature (data/path assumptions) and isn't in scope. Stating it keeps the details panel honest (template shown read-only). |
| 6 | **`build-rootfs.sh` grows independent language-layer flags: `--go` / `--node` / `--python`, each default-off; the platform baseline stays always-on.** Go (currently always-on) becomes a flag (kept on in the current default invocation for back-compat). Python adds `python3`, `pip`, `venv`, `build-essential`, all sha/source-pinned and recorded in `manifest.lock`. | The platform-vs-language model (Context) becomes literal in the builder. Independent flags make the five templates pure combinations rather than divergent scripts (`base` = no language flag; `full` = all three). Pinning Python the same way as Go/Node keeps the SHA-keyed, reproducible-image rule intact (the "anything on the host is in the playbook" rule). |
| 7 | **Image naming carries the template id: `proteos-rootfs-<template>-<base>-ga<sha>.ext4`; a new `image/bake-templates.sh` (and a Taskfile task) bakes the whole set** by invoking `build-rootfs.sh` once per template with its flag set, emitting one `manifest.lock` per image. | The default name (`…-<base>-ga<sha>`) can't distinguish go/node/python builds that share a base+SHA. The orchestrator script is where the catalog's `rootfs_ref`s come from — its output names are exactly what goes into `PROTEOS_TEMPLATES_FILE`. |
| 8 | **Ansible `firecracker` role loops over the template set, ensuring each image is baked/present in every host's `images_dir`** (existence-guarded like the current single rootfs; force-rebake by deleting that image's `manifest.lock`). No scheduler/affinity change. | The user chose "every host has every image", so this is purely an Ansible fan-out over the existing single-image task. Keeps the "any machine lands on any host" invariant true without touching placement logic. |
| 9 | **Frontend: (a) create dialog with a template picker + collapsible "Advanced" resource overrides pre-filled from the chosen template's defaults; (b) a read-only Machine Details panel** (template label, vcpus/mem/disk, state, guest_ip, boot, created_at) reachable from the switcher. Resource inputs are bounded by the `limits` from `GET /api/templates`. | Turns the bare "+ New machine" button into the selection flow and finally renders the `MachineSummary` fields that already arrive but were never shown ("machine details" backlog item). Defaults-prefilled + bounded inputs keep the common path one click while making override discoverable. |

## Wire contracts

```
GET /api/templates                         (requireAuth)
  → 200 [ { "id":"go", "label":"Go development", "description":"…",
            "defaults": {"vcpus":2,"mem_mib":2048,"disk_mib":10240},
            "limits":   {"vcpus":{"min":1,"max":8},
                         "mem_mib":{"min":1024,"max":16384},
                         "disk_mib":{"min":5120,"max":51200}} }, … ]

POST /api/machines                          (requireAuth)
  body: { "name"?: string,                  // empty ⇒ auto-named (unchanged)
          "template_id"?: string,           // empty ⇒ catalog default
          "vcpus"?: int, "mem_mib"?: int, "disk_mib"?: int }  // empty ⇒ template default
  → 202 MachineSummary   (now also includes "template_id")
  → 400 unknown_template
  → 400 invalid_resources   { "detail": "vcpus must be 1..8" }
  → 409 machine_limit       (unchanged)

MachineSummary  (web/src/api/client.ts) gains:
  "template_id": string | null            // null ⇒ legacy machine (pre-templates)
```

## Tracer-bullet slices

Sequenced so each slice is independently testable; plumbing lands and is verifiable with
the **existing** image as the sole catalog entry before any slow bake work.

- **Slice 1 — Catalog + create wiring (no UI). ✅ DONE (2026-06-19).** `machine.Template`/
  `Resources`/`Catalog` + `PROTEOS_TEMPLATES_FILE` loader + `SingleTemplateCatalog` fallback
  + boot validation (`internal/machine/template.go`); migration `000007` (`template_id`,
  nullable); `Service.Create` takes `CreateOptions{Name,TemplateID}`, resolves
  template→refs+defaults via `resolveCreate`, stamps `template_id`; `ensureOnAgent` now
  sizes vCPU/mem from the machine's own `resource_spec` (was the global Spec); `GET
  /api/templates` (`internal/httpapi/templates.go`) omitting refs; create maps unknown
  template → `400 unknown_template`; `MachineSummary.template_id` added. Empty catalog =
  legacy path (tests). Unit tests (catalog validation/load/parse) + DB tests (default/named/
  unknown template create) green; `go build/vet`, `sqlc diff`, `gofmt` clean. *Empty-body
  create unchanged in behaviour.* Remaining: per-resource override + caps land in Slice 2.
- **Slice 2 — Resource override + validation. ✅ DONE (2026-06-19).** `ResourceLimits`/`Bound`
  + `NewResourceLimits` (fixed floors 1/1024/5120, maxes from `PROTEOS_MAX_VCPUS`/`_MEM_MIB`/
  `_DISK_MIB` defaulting 8/16384/51200) + `InvalidResourcesError{Detail}` in `template.go`;
  `CreateOptions` gained `Vcpus`/`MemMiB`/`DiskMiB *int`; `resolveCreate` applies per-dimension
  overrides onto the template defaults then bound-checks (empty limits ⇒ skip, for tests);
  startup validates every template's defaults are within caps (fails loudly otherwise). HTTP:
  create decodes the override fields and maps `InvalidResourcesError`→`400 invalid_resources`
  with a `detail` (errorEnvelope gained an omitempty `detail`); `GET /api/templates` now carries
  a global `limits` block per entry. Tests: unit (limits matrix), DB (override within caps incl.
  disk sizing, partial override, out-of-caps), HTTP (templates shape + no-ref leak, invalid_resources
  detail, unknown_template). `go build/vet`, `sqlc diff`, `gofmt` clean; full suite green
  (serialized — parallel package runs can contend on Testcontainers Postgres, a pre-existing infra
  quirk; CI's shared `TEST_DATABASE_URL` avoids it).
- **Slice 3 — Frontend. ✅ DONE (2026-06-19).** `api.getTemplates()` + `useTemplates()` query;
  `MachineTemplate`/`CreateMachineInput` types; `MachineSummary.template_id` added; `createMachine`
  takes the input object; `ApiError` now carries `detail` (for `invalid_resources`). New components:
  `Modal` (shared overlay, Esc/backdrop close), `CreateMachineDialog` (template radio picker showing
  per-template default specs + optional name + collapsible Advanced resource inputs bounded by each
  template's `limits`, resetting to the picked template's defaults on change; inline machine_limit/
  unknown_template/invalid_resources errors), `MachineDetails` (read-only: template label, vCPUs/mem/
  disk, **RootFS image**, state, guest IP, boot, created). Wired into the Taskbar `MachineSwitcher`
  (menu gained Details + opens the dialog instead of insta-creating). CSS for modal/form/details added.
  Gates (run directly via node — the `npm` wrapper and `vite build` can't spawn under the nono
  sandbox): **tsc 0, eslint 0 errors, knip 0, vitest 25/25, prettier 0**. The production bundle
  (`vite build`) is unverifiable here — it fails identically on the pristine tree (sandbox blocks the
  bundler's native subprocess), so it must be confirmed in CI / outside the sandbox.
- **Slice 4 — Bake. ✅ SCRIPTS DONE (2026-06-19); bake pending FC host.** `build-rootfs.sh`
  gained `--node`/`--no-node` (force the Node runtime independently of the npm providers;
  `--no-node` errors if a provider is also requested), `--python`/`--no-python` (new
  `install_python`: extract-only `python3`+`pip`+`venv`+`python3-dev`+`build-essential` via the
  existing `apt_extract_install`, plus the cc/gcc/c++/g++ alternative symlinks the skipped
  postinst would make; sets `PYTHON_VERSION`; +600 MiB headroom), and `--template <id>`
  (names the image `proteos-rootfs-<id>-<base>-ga<sha>.ext4` and the manifest
  `manifest-<id>.lock`, so a multi-template bake shares one out-dir; `template`/`python_version`
  added to the manifest + `PROTEOS_TEMPLATE`/`PROTEOS_PYTHON_VERSION` to `/etc/proteos-release`;
  empty `--template` = legacy single-image name, back-compat). New `image/bake-templates.sh`
  orchestrator bakes the standard set (base=`--no-go`, go=`--go`, node=`--no-go --node`,
  python=`--no-go --python`, full=`--go --node --python`), forwarding any post-`--` flags (e.g.
  `--claude-bootstrap`) to every bake and printing the baked image names to register in
  `PROTEOS_TEMPLATES_FILE`. Verified: `bash -n` clean on both; flag parsing, the
  `--no-node`+provider conflict, unknown-template-id, and per-template dispatch all tested on
  macOS (dispatch correctly stops at build-rootfs.sh's "run it on Linux" loop-mount guard).
  **The actual bake must run on the Linux Firecracker host** (loop-mount + KVM + apt) — can't run
  in this sandbox. No Taskfile task added (the bake is an ops action on the FC host, not a dev
  task); run `image/bake-templates.sh` directly.
- **Slice 5 — Deploy + register + acceptance.**
  - **Ansible bake-the-set ✅ DONE (2026-06-19; runs on FC host).** Corrected scope: the bake
    lives in the **node_agent** role (not firecracker), and this playbook configures only the
    fc-node — the control plane is a separate app-stack deploy. `group_vars` gained a declarative
    `proteos_templates` catalog (base/go/node/python/full, each with go/node/python flags + label/
    description/default resources), `proteos_primary_template`, and `proteos_templates_fetch_dir`.
    The single bake task became a **loop over `proteos_templates`**, each calling
    `build-rootfs.sh --template <id>` with its language flags; the platform layer (claude/git/vim/
    taskfile/code-server/user/aliases) stays common, and the npm providers ride the Node layer
    (baked only when `node: true`). Per-template rebake guard (`manifest-<id>.lock` missing or
    source moved). Node/Go bake-prereq apt guards widened to "any template needs it". After baking,
    the role reads each `manifest-<id>.lock`, builds an id→image map, pins `PROTEOS_ROOTFS_REF` to
    the primary, asserts every image exists, **renders `proteos-templates.json`** (the control
    plane's `PROTEOS_TEMPLATES_FILE`, kernel_ref omitted ⇒ filled from `PROTEOS_KERNEL_REF`) and
    **fetches it to the controller** (`artifacts/`, gitignored) for the app-stack deploy. README +
    group_vars docs updated (per-template `manifest-<id>.lock`). Verified: `ansible-playbook
    --syntax-check` clean; `ansible-inventory` resolves the 5-template set on the host; the
    id→image extraction + catalog JSON validated in Python; **the exact generated catalog loads
    through the real Go `LoadCatalogFile` and passes the startup caps validation**. Could NOT run
    here: the playbook tasks (sandbox blocks `/dev/shm`) and the real bake (needs the Linux FC
    host) — both run on the provisioning machine.
  - **App-stack wiring ✅ DONE (2026-06-19).** `deploy/app-stack/compose.yaml`: the control plane
    gained `PROTEOS_TEMPLATES_FILE` (default empty ⇒ synthesized `base` fallback, so a fresh stack
    works), `PROTEOS_MAX_VCPUS`/`_MEM_MIB`/`_DISK_MIB` (8/16384/51200), and `PROTEOS_MACHINE_DISK_MIB`
    (was in `.env.example` but never passed through — fixed); the catalog is mounted read-only at
    `/etc/proteos/templates.json` from a committed placeholder (`{"templates":[]}` + a `_comment`)
    following the `openbao-secret-id` pattern. `.env.example` documents the activation flow; `RUNBOOK`
    Part B2 got a "machine templates (optional)" block (copy the fetched catalog over the placeholder,
    set the env, mind the caps). DEPLOYMENT.md left untouched (it's the stale PoC guide). Verified:
    `docker compose config` renders the envs + mount with the file both set and unset; placeholder is
    valid JSON and **fails loudly through the real Go loader** (`template catalog is empty`) if
    activated unreplaced — never a machine booting a bogus image; control plane still builds.
  - **Remaining (needs the provisioned host):** run the playbook to bake the set, copy the fetched
    `proteos-templates.json` onto the app VM + set `PROTEOS_TEMPLATES_FILE`, then live end-to-end
    acceptance (create a machine per template, confirm the toolchain in the VM and the details panel).
    The two Slice 4 caveats (build-essential extract-only, `full` image size) still want a first real
    bake to confirm.

## Touch list (anticipated)

- `controlplane/internal/machine/` — `Template`/catalog, resolution+validation in `Create`.
- `controlplane/internal/config/config.go` — `PROTEOS_TEMPLATES_FILE`, cap envs.
- `controlplane/internal/httpapi/` — `GET /api/templates`, extended create decode, `template_id` in summary.
- `controlplane/migrations/000007_template_id.{up,down}.sql`; `store/queries.sql` + `models.go`.
- `web/src/api/client.ts` + `hooks.ts` — `getTemplates`, `createMachine` args, `template_id` field.
- `web/src/desktop/Taskbar.tsx` (+ new create dialog + details panel components).
- `image/build-rootfs.sh`, new `image/bake-templates.sh`, `Taskfile.yaml`.
- `deploy/ansible/roles/firecracker/tasks/main.yml`, `group_vars/all.yml`.
- `DEPLOYMENT.md` / `RUNBOOK.md`.
