# Technical decisions

Each entry: decision, why, and what would make us revisit it. Newest at the bottom.

## D-001 License: AGPL-3.0 (2026-11-06)
Keeps the project open while making sure hosted forks share their changes.

## D-002 MVP runs on our own Hetzner account (2026-11-06)
Best user experience and the only option where we can promise recovery targets. Nodes join through a provider-agnostic bootstrap, so customer Hetzner accounts (same code, different API token) can follow. Customer-owned machines are out of scope.
Revisit if: design partners need data in their own cloud account.

## D-003 One k3s cluster per tenant on per-tenant VMs (2026-11-06)
The hypervisor is the isolation boundary between tenants. No shared kernel or Kubernetes API between organisations.
Revisit for: short-lived job workloads with many small tenants, which need a shared-pool runtime (gVisor or microVMs) instead.

## D-004 Node OS: Debian (2026-11-06)
Simplest path for the MVP. Immutable OS options (Talos, bootc) can be revisited later.

## D-005 Pull-based tenant agent (2026-11-06)
The control plane never connects into tenants. Tenants keep running when the control plane is down. Flux vs custom agent is still open.

## D-006 Hetzner Volumes + CloudNativePG + off-location backups + nightly restore tests (2026-11-06)
Hetzner documents triple replication for volumes, which removes the need for our own distributed storage. Restore tests turn backups from a guess into a checked fact.

## D-007 Domains: slicedbread.dev for console, slicedbread.cloud for apps (2026-11-06)
Separate registrable domain for user content. slicedbread.cloud goes on the Public Suffix List.

Amended 2026-11-06: development and tests use `test.slicedbread.dev` until launch. Revisit before launch, because a zone-scoped test token can edit every record in `slicedbread.dev`.

## D-008 DNS on Cloudflare, wildcard certs per tenant via DNS-01 (2026-11-06)

## D-009 Own login wall, GitHub login first, per-app access modes (2026-11-06)
Modes: `wall` (default), `app-auth`, `public`, plus public paths. Session cookie is stripped before requests reach apps. Vaultwarden uses `app-auth` because Bitwarden clients cannot pass a browser login wall.

## D-010 Patching: unattended-upgrades + kured, node replacement through the Hetzner API (2026-11-06)

## D-011 Users bring their own agent: MCP server, CLI, AGENTS.md (2026-11-06)
Their agent already knows their codebase, and we carry no LLM cost. Exposure changes and destructive actions need human approval. A built-in agent for background operations comes later.

## D-012 Lovable exports: run the app, keep Lovable Cloud as the backend (2026-11-06)
Quick to demo. The user's agent can help migrate the Supabase backend later.

## D-013 Templates: our own compose-like spec, adapted from Coolify (Apache-2.0) and Dokploy templates (MIT) (2026-11-06)
Every template passes an agent-run verification workflow before it ships. Do not copy from the main Dokploy repository (non-standard license). Keep attribution for adapted templates. Builds use Railpack (Nixpacks is in maintenance mode).

## D-014 Prototype: sb-control runs inside the tenant cluster (2026-11-06)
For the prototype the control plane, the login wall's forward-auth service and the reconciler are one service (`sb-control`) running in each tenant. It reconciles directly against the local Kubernetes API, so the pull-based agent from D-005 is not needed yet. State lives in Kubernetes: `App` and `Approval` custom resources, users and API tokens in a secret.
Revisit when: we add a multi-tenant control plane at slicedbread.dev.

## D-015 Own login wall with forward-auth and cookie stripping (2026-11-06)
Users sign in with email and password at `auth.<tenant-domain>` (GitHub login comes next and needs an OAuth app). Sessions are HMAC-signed cookies scoped to the tenant domain. Every app routes through a Traefik forwardAuth middleware: wall apps through `/auth/verify`, other apps through `/auth/strip`. Both list `Cookie` in `authResponseHeaders`, and Traefik deletes listed headers before copying the auth response, so the session cookie never reaches an app. Verified live: apps receive only their own cookies plus `X-Sb-User`.

## D-016 Agent and admin tokens; approvals for widening exposure and deletes (2026-11-06)
The CLI uses an agent token. Creating or changing an app to `public` or `app-auth` (unless the template defines it), adding public paths, settings listed as widening exposure (Vaultwarden signups), and deletes become `Approval` objects. A human approves at `auth.<tenant-domain>/approvals` or with an admin token. The CLI exits with code 3 for pending approvals.

## D-017 Builds: Railpack frontend on in-cluster BuildKit, in-cluster registry (2026-11-06)
Repos and uploaded directories are built by a Job (clone or fetch, `railpack prepare`, `buildctl` with the Railpack frontend, or the project's Dockerfile) against a BuildKit daemon in `sb-builds`, and pushed to a registry at a fixed ClusterIP. Nodes pull through a containerd mirror (`sb-registry.local:5000`). BuildKit is privileged and only reachable from the build namespace. When both `package-lock.json` and a bun lockfile exist, the build uses npm, because Lovable exports often carry a stale `bun.lockb`.

## D-018 Directory uploads (2026-11-06)
`sb deploy --dir` uploads a gzip tarball (dependencies and build output skipped) that sb-control stores on its volume. Build jobs fetch it over an HMAC-signed in-cluster URL. This lets an agent ship local edits without pushing to git.

## D-019 Prototype tenants use production Let's Encrypt (2026-11-06)
`sb dev prototype up` issues trusted certificates so browsers and Bitwarden clients work. Smoke tests stay on staging.

## D-020 sb-control image is built locally and imported over SSH (2026-11-06)
go-containerregistry packs the static binary onto a distroless base, and the tarball is imported into containerd on each node. No external registry or extra credentials. `sb dev control push <run-id>` rolls out a new build in about a minute.

## D-021 Self-hosted mode is the first release (2026-11-06)
Users run `sb init` from their laptop with their own Hetzner and Cloudflare tokens and their own domain, choosing 1 node (cheap, no HA) or 3 nodes (HA). The control plane runs inside their cluster. Hosted-by-us starts as a waitlist; bring-your-own-servers comes later. Details in [hosting-modes.md](hosting-modes.md).
Why: it is what exists, it needs no billing or abuse handling, and it fits the open-source, no-lock-in positioning.

## D-022 TLS termination depends on the mode (2026-11-06)
Self-hosted: TLS ends inside the user's cluster, one wildcard per tenant via DNS-01, keys never leave it; add ZeroSSL as a fallback issuer (Google Trust Services was considered and dropped: it needs a Google Cloud account). Hosted by us: we terminate TLS at a shared edge with flat hostnames (`<app>-<tenant>.slicedbread.cloud`) covered by one wildcard. The MVP keeps hosted mode possible: hostnames from one function, host-only cookies with a per-host session exchange, forwarding headers trusted only from the edge, uploads not capped at 100 MB.

## D-023 Builds run in their own rootless BuildKit; apps deploy by digest (2026-11-06)
Replaces the shared privileged BuildKit daemon of D-017. Each build is a Job running `moby/buildkit:<v>-rootless` with `buildctl-daemonless.sh` as uid 1000, not privileged, with `--oci-worker-no-process-sandbox` and unconfined seccomp and AppArmor (needed for its user namespace). The layer cache lives in the registry per app (`<app>:buildcache`), so builds stay fast without a shared daemon. The build records the pushed digest and the Deployment uses `<app>:<build>@sha256:...`. The registry admits only `sb-builds` pods and the nodes (a NetworkPolicy sb-control rebuilds from the node list: node IPs and each node's pod CIDR .0 and .1), has deletes off, and is cleaned daily by a job that keeps the digests apps run and the manifests they reference.
Why: a build escape from a privileged shared daemon was root on the node, and any app pod could overwrite another app's image tag in the registry.

## D-024 Members, roles and who approves (2026-11-06)
Users are admins or members. Admins invite people with one-time links (7 days; SlicedBread does not send email), change roles and remove members; an install always keeps at least one admin. Members pass the login wall. Whether members may approve risky changes is an install-wide setting only admins change, off by default. A solo admin approves their own agent's requests. Every session check confirms the user still exists, so removing a member ends their sessions immediately; agent tokens record who approved them and are revoked with that person. GitHub sign-in is deferred: on self-hosted installs every user would need their own registered OAuth app.
