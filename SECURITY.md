# Security Policy

ProteOS runs untrusted, autonomous AI coding agents inside hardware-isolated
Firecracker microVMs. The isolation boundary is the core security property of the
project, so we take vulnerability reports seriously — especially anything that
could let an agent escape its microVM, cross between users/machines, or reach the
control plane, gateway, or other tenants.

## Reporting a vulnerability

**Please do not report security issues through public GitHub issues, pull
requests, or discussions.**

Instead, report privately through one of:

- **GitHub Security Advisories** (preferred) — use the
  [**Report a vulnerability**](https://github.com/tavon-ai/proteos/security/advisories/new)
  button on the Security tab. This opens a private channel with the maintainers.
- **Email** — `security@tavon.ai` _(maintainers: replace with a monitored
  address before publishing if this mailbox does not exist)_.

Please include, as far as you can:

- a description of the issue and its impact;
- the affected component (control plane, node-agent, guest-agent, gateway, CLI,
  web) and version/commit;
- step-by-step reproduction, proof-of-concept, or affected code paths;
- any suggested remediation.

## What to expect

- We aim to **acknowledge** a report within **3 business days**.
- We'll work with you to confirm the issue and determine its severity and scope.
- We'll keep you updated on remediation progress and coordinate a disclosure
  timeline. We ask that you give us a reasonable window to ship a fix before any
  public disclosure.
- With your consent, we're happy to credit you in the advisory and release notes.

## Scope

Areas of particular interest:

- microVM isolation / guest-to-host escape
- cross-tenant access (one user/machine reaching another's data or VM)
- authentication and authorization on the control plane and gateway
- secret handling (OpenBao integration, provider API keys, GitHub tokens)
- the port-preview and editor gateway paths

Reports about dependencies should ideally include how the vulnerable code path is
reachable in ProteOS.

Thank you for helping keep ProteOS and its users safe.
