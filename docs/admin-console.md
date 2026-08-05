# Admin console

A fleet-wide, **read-only** view of every machine and who owns it: how many
machines exist, how many are in use, and which user each one belongs to.

Reachable from the **Admin** item in the desktop's left rail, which is visible
only to holders of the `proteos.admin` role. The window is a single screen —
headline totals, a state histogram, and a machine table grouped by owner.

## What it shows

| Number | Means |
| --- | --- |
| **Machines** | Every machine row that exists, in any state. |
| **In use** | Machines in the `running` state — the ones consuming host CPU and memory. |
| **Owners** | How many users own at least one machine, out of the total account count. |

The state chips below the totals break the rest of the fleet down (`stopped`,
`error`, and the transient states). Machines are then listed under their owner,
heaviest users first, with size, host, age, and last activity.

The totals are counted **separately from the table**, so they stay exact even
when the table is capped (500 machines). When that happens the window says so
explicitly — a truncated list never passes for a complete one.

## Why it has no actions

The console is deliberately incapable of mutation. It is the one screen in the
product where a user sees rows that are not theirs, and keeping it read-only
means the blast radius of a mistakenly-granted role is "saw a machine list",
not "destroyed someone's work". Admin *actions*, if they are ever wanted,
should be added deliberately and with their own audit trail — not by widening
these endpoints.

## Granting access

Access is a Zitadel project role, `proteos.admin`, on the shared `Tavon.io`
project. The key is namespaced because that project's roles claim is shared
with databox, chat, and harness; the control plane matches this key exactly, so
another application's role can never confer ProteOS privilege.

```bash
# Create the role (idempotent) and enable the roles claim on the project.
ZITADEL_PAT=xxx ./scripts/zitadel/register-proteos-admin-role.sh

# Grant / revoke / list.
ZITADEL_PAT=xxx ./scripts/zitadel/register-proteos-admin-role.sh --grant ivan@example.com
ZITADEL_PAT=xxx ./scripts/zitadel/register-proteos-admin-role.sh --revoke ivan@example.com
ZITADEL_PAT=xxx ./scripts/zitadel/register-proteos-admin-role.sh --list
```

The script only ever **adds** to a user's grant on the shared project, so
granting ProteOS admin never disturbs the roles they hold for the other apps.

> **A change takes effect on the user's next sign-in.** Tell them to sign out
> and back in.

### Why the delay

ProteOS sessions are opaque and server-side: the IdP is consulted once, at
login, and never again for an ordinary API call. There is therefore no
per-request token to re-read a role claim from. The role is captured from the
userinfo claims at login and mirrored onto `users.is_admin`, which is what the
`requireAdmin` middleware consults.

This is a deliberate trade — it adds no per-request IdP traffic and no new
machinery — and it cuts both ways: a **revoked** grant also persists until the
user's next sign-in. To cut someone off immediately, revoke the grant and
revoke their ProteOS sessions.

## Where it lives

| Piece | Path |
| --- | --- |
| Role key + rationale | `controlplane/internal/auth/roles.go` |
| Claim parsing | `controlplane/internal/oidc/client.go` (`UserInfo.HasRole`) |
| Role persistence | `controlplane/internal/auth/auth.go` (`resolveOIDCUser`) |
| Route gate | `controlplane/internal/httpapi/middleware.go` (`requireAdmin`) |
| Endpoint | `controlplane/internal/httpapi/admin.go` (`GET /api/admin/overview`) |
| Window | `web/src/windows/AdminWindow.tsx` |
| Rail item | `web/src/desktop/LeftRail.tsx` |

Hiding the rail item is a courtesy, not the access check: `/api/admin/*` answers
`403` to every non-holder regardless of what the client believes.
