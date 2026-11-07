# Architecture

Status: draft, 2026-11-06. Decisions referenced as D-NNN live in [decisions.md](decisions.md).

## Goals

1. Host off-the-shelf software as easily as Coolify or Dokploy, without the user managing security.
2. Survive a node failure with no data loss and no manual recovery.
3. Put every app behind a login wall by default, with explicit, declared exceptions.
4. Let the user's own coding agent deploy and configure apps through an MCP server, a CLI and an `AGENTS.md`.

## Non-goals for the MVP

- Running on customer-owned machines or customer cloud accounts (D-002).
- Multiple regions per tenant.
- Mail, authoritative DNS and other services that need a fixed public IP and port.
- Billing, autoscaling, identity providers other than GitHub.
- Migrating Lovable Cloud backends (D-012).

## Overview

```
                    slicedbread.dev                          slicedbread.cloud
 ┌───────────────────────────────────────┐     ┌──────────────────────────────────────────────┐
 │ Control plane                         │     │ Tenant "acme" (own Hetzner VMs, own k3s)      │
 │                                       │     │                                              │
 │  console / API / OIDC issuer          │     │  Hetzner LB :443 ──► Traefik                  │
 │  GitHub login                         │     │                       │ forward-auth          │
 │  tenants, apps, desired state         │◄────┼── tenant agent        ▼                       │
 │  MCP server, CLI backend              │ pull│   (outbound only)   auth proxy ──► apps       │
 │  renders manifests per tenant         │     │                                              │
 └───────────────────────────────────────┘     │  CNPG Postgres, Hetzner Volumes (CSI)         │
                                               │  Build jobs: rootless BuildKit + Railpack     │
                                               │  backups ──► object storage (other location)  │
                                               └──────────────────────────────────────────────┘
```

The control plane never connects into a tenant. Each tenant runs an agent that connects out, pulls its desired state and reports status (D-005). If the control plane is down, tenants keep serving.

## Tenancy and isolation (D-003, D-004)

- **One tenant = one org = its own Hetzner VMs and its own k3s cluster.** The boundary between tenants is the Hetzner hypervisor. No shared kernel, no shared Kubernetes API.
- **Tenant shape:** 3 small Debian VMs, each a k3s server and worker (embedded etcd survives the loss of one node), on a Hetzner private network.
- **Inside a tenant:** one namespace per app, default-deny NetworkPolicy between namespaces, Pod Security Admission `restricted`, no hostPath, no privileged pods. A compromised app should not be able to reach another app's database.
- **Builds run inside the tenant's own cluster.** Building a repo executes untrusted code, so it stays inside that tenant's isolation.
- **Network exposure:** a Hetzner Cloud Firewall allows only 80/443 to the load balancer. The Kubernetes API and SSH are not public.

## Domains and TLS (D-007, D-008)

- `slicedbread.dev`: website, docs, console, API, OIDC issuer.
- `slicedbread.cloud`: tenant apps at `<app>.<tenant>.slicedbread.cloud`. User content lives on a separate registrable domain from the console.
- `slicedbread.cloud` will be submitted to the Public Suffix List, so a tenant cannot set cookies for another tenant. Check whether Let's Encrypt then applies rate limits per tenant subdomain.
- DNS on Cloudflare. One wildcard certificate per tenant (`*.<tenant>.slicedbread.cloud`) issued by cert-manager with DNS-01.
- Custom domains later via a delegated subdomain such as `*.tools.example.com`.
- **Development and tests** run under `test.slicedbread.dev`: `<app>.<run-id>.test.slicedbread.dev`, with Let's Encrypt staging certificates. Cloudflare API tokens are scoped per zone, so a test token can edit any record in `slicedbread.dev`. Before launch, either move tests to a separate domain or keep the console off `slicedbread.dev`.

## Login wall (D-009)

Each request to an app passes through Traefik forward-auth to the tenant's auth proxy. The control plane is the OIDC identity provider, with GitHub as the first upstream login.

Each app declares an **access mode**:

| Mode | Behaviour | Example |
|---|---|---|
| `wall` (default) | Every request needs a SlicedBread session for a member of the tenant | Metabase, n8n, internal Lovable apps |
| `app-auth` | No wall. The app's own login protects it. Template must apply hardening defaults | Vaultwarden (Bitwarden apps and extensions cannot pass a browser login wall) |
| `public` | No wall, no expectation of app login | Marketing sites |

`wall` apps can declare **public paths** that bypass the wall, for example Umami's tracker script and event endpoint.

Rules:

- **Strip the SlicedBread session cookie before forwarding upstream.** Otherwise any app in the tenant receives it and a compromised app could replay it against the tenant's other apps.
- Pass identity to the app as signed headers (user id, email, GitHub login), never the session itself.
- Changing an app to `app-auth` or `public` is a privileged action and needs human approval when requested by an agent (D-011).

## Storage, backup and recovery (D-006)

- **App volumes:** Hetzner Cloud Volumes through the Hetzner CSI driver. Hetzner documents triple replication of every block. When a node dies, the pod reschedules and the volume reattaches to another node in the same location. Verify attach limits and reattach timing during M0.
- **Postgres:** CloudNativePG with one replica, continuous WAL archiving to object storage, point-in-time recovery.
- **Backups:** nightly volume backups and CNPG base backups to object storage in a different location from the cluster.
- **Restore tests:** every night, the latest backup of each app is restored into a scratch namespace and checked (template-defined check). A failed restore test raises an alert.

Proposed recovery targets (to confirm):

| Failure | Recovery time | Data loss |
|---|---|---|
| One node | under 10 minutes, automatic | none |
| Whole Hetzner location | under 4 hours | at most 1 hour of Postgres, 24 hours of volume data |

## Node lifecycle (D-010)

- Debian with unattended security upgrades.
- kured drains and reboots nodes one at a time when a reboot is required.
- A node that stays NotReady past a threshold is replaced by the control plane: cordon, delete the VM through the Hetzner API, create a new VM with cloud-init, join it to the cluster.
- App image updates roll out with health checks and roll back automatically on failure.

## Builds and deploys (D-013)

- Sources: catalog template, container image, or git repo through a GitHub App.
- Repos are built with Railpack. If detection fails, the user's agent can add a Dockerfile.
- Lovable exports run on SlicedBread while still using Lovable Cloud as their backend, configured through env vars. Backend migration is out of scope (D-012).

## Desired state and the tenant agent (D-005)

The control plane stores desired state per tenant and renders it into Kubernetes manifests. The tenant agent pulls the rendered state, applies it and reports health back.

Open: use Flux pulling a per-tenant OCI artifact, or a small custom agent. Flux is proven. A custom agent gives us status reporting and actions in one place. Decide during M1.

## Agent interface (D-011)

- **MCP server, CLI and `AGENTS.md`** expose the same API. Users bring their own agent.
- Tools for the MVP: list catalog, deploy app, get app (URL, status, credentials reference), set env, link apps (inject one app's URL or key into another), run template action, logs, redeploy.
- Agent tokens are scoped to one tenant.
- Deploys and config changes apply directly. Destructive actions (delete, restore over live data) and exposure changes (`app-auth`, `public`) need human approval.
- A built-in agent for background operations (triaging failed restore tests, explaining incidents) comes later.

## Open questions

- Auth proxy implementation: oauth2-proxy, Pomerium, or a small custom service. Custom is likely, given cookie stripping, access modes and public paths.
- Object storage provider for backups: Hetzner Object Storage in a second location, or Cloudflare R2 for provider separation.
- Flux or custom tenant agent (see above).
- Minimum observability for the MVP: what alerts exist on day one.

## Prototype implementation (2026-11-06)

What is built today, and where it differs from the target design above:

| Area | Prototype |
|---|---|
| Control plane | `sb-control` inside each tenant (D-014). No central control plane yet. |
| Login wall | Email and password accounts with admin and member roles and invite links (D-024), forward-auth with cookie stripping (D-015). GitHub sign-in deferred. |
| Agent safety | Agent and admin tokens, approvals for widening exposure and deletes (D-016). |
| Sources | Templates (Vaultwarden, Umami), container images, public git repos, uploaded directories (D-017, D-018). |
| Databases | CloudNativePG, one instance per app. No backups yet. |
| Backups and restore tests | Not built (M6). Do not put real data on a prototype tenant. |
| Node replacement | sb-guard (D-026, D-029), two replicas with a lease: a server Hetzner reports off is fenced within one 5-second check (out of the load balancer, out-of-service taint); a server Hetzner reports off or gone is fenced with the out-of-service taint, deleted and recreated with the same name and private IP; a server that runs but stays NotReady gets one hard reset after 5 minutes, then replacement. 3-node installs only, at most 3 per day. |
| Patching | sb-guard (D-028): unattended security upgrades on every server; nightly, a server that needs a reboot is cordoned, its stateless apps surge-restarted elsewhere, the rest moved, taken out of the load balancer, rebooted and put back, one server at a time. Template image updates are reported. |
| Control plane availability | sb-control runs two replicas without a volume on 3-node installs (D-027). |
| Known gaps | Apps run as root in containers with all capabilities dropped (Pod Security `baseline`, warn on `restricted`). The in-cluster registry has no login (only build pods and nodes can reach it; apps run by digest). Build pods run with unconfined seccomp and AppArmor for rootless BuildKit. |
