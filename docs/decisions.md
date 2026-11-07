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

## D-025 Humans use the dashboard, agents use the CLI (2026-11-06)
Every API token has agent rights. `sb init` creates no admin user and no admin token: it prints a one-time setup link (a code whose hash sits in `sb-system/sb-setup`, 24 hours, single use, `sb setup-link` makes a new one) where the person who ran it creates the owner account, and leaves the CLI logged in with an agent token. Approvals, members, backup settings and the encryption password, restoring every app from the bucket, alert webhooks, export, admin-only actions and admin tokens for automation (named, expiring within 90 days) are dashboard pages; the CLI commands for them print the dashboard link. `sb init` generates the backup encryption password, installs it in the cluster and prints it once without writing it to disk; configuring a bucket later reuses it. Let's Encrypt issues without an email; the ZeroSSL fallback is registered with the owner's email after setup, and sb-control moves the wildcard certificate to it when Let's Encrypt has not issued for 30 minutes or the certificate is within 20 days of expiry.
Why: a coding agent on the same machine used the CLI's admin token and could approve its own requests; separating the surfaces lets a human and an agent share one CLI safely.

## D-026 sb-guard replaces dead servers on 3-node installs (2026-11-06)
A separate Deployment (same binary, `SB_MODE=guard`, no volume, tolerates an unreachable node for only 20 seconds) watches nodes. When exactly one of three nodes is NotReady: if Hetzner reports its server off or gone for 30 seconds, the guard taints the node `node.kubernetes.io/out-of-service` (pods are deleted and volumes detached at once instead of after about 6 minutes), deletes the server and the Node (k3s then removes its etcd member and node password), and creates a server with the same name, labels, firewall, placement group and private IP, joined with the token stored at install in `sb-system/sb-join`. If Hetzner reports the server running, it gets one hard reset after 5 minutes and is replaced if still NotReady 8 minutes later. Never more than one node at a time, never more than 3 replacements a day, nothing on 1-node installs. The sb-control image is also pushed into the in-cluster registry so a new server can pull it. Progress is kept in `sb-system/sb-node-repair` (a restarted guard resumes) and shown as alerts; `GET /v1/nodes` gives the CLI the new address and pinned host key.
Why: a guard inside sb-control would die with sb-control's server, and sb-control's volume stays attached to a dead server until something fences it.

## D-027 sb-control runs two replicas without a volume (2026-11-06)
Every login-walled app asks sb-control on each request, so a single sb-control pod with a ReadWriteOnce volume took every app down while its server rebooted or died. On 3-node installs sb-control now runs two replicas (one on 1-node installs) with a disruption budget and leaves an unreachable server after 30 seconds. Uploaded source lives in the in-cluster registry (`sb-uploads`), auth state in the `sb-auth` Secret updated with resourceVersion checks, one-time login exchanges as ConfigMaps that can be created once, CLI login grants as Secrets. Background work (builds, backups, alerts, registry upkeep) runs on the replica holding the `sb-control` Lease. On shutdown a replica reports not-ready and keeps serving for 6 seconds.

## D-028 OS maintenance by sb-guard (2026-11-06)
An hour after the backup hour (or when an admin presses "Run OS maintenance now"), sb-guard runs a privileged Job on each server that enables Debian's unattended-upgrades for security updates (never rebooting by itself) and reports `/run/reboot-required`. Servers that need it are rebooted one at a time: cordon, surge-restart Deployments without volumes so the new pod serves elsewhere first, delete the remaining pods (volumes move), take the server out of the load balancer by changing its `sb/role` label, reboot, wait for a new boot ID and Ready, uncordon, put it back and wait for healthy. The guard's own server goes last; a fresh guard finishes it. Node repair leaves a server alone while it is in planned maintenance. Template image tags are compared with upstream tags daily and newer versions are reported in `sb status` and on the dashboard.

## D-029 Template actions run as container jobs; multi-component apps come with the template format (2026-11-07)
A template action is a script in the template, run as a short-lived Job next to the app with the app's internal address and only the secrets the action declares. It prints one JSON object, which is checked against the action's declared output. Actions no longer live as Go code in sb-control. A few built-in behaviours stay in sb-control because they must keep running after an action returns (for example closing Vaultwarden signups after 15 minutes). The template format is designed around several components per app (replicas, config files, routes) from the start, so existing templates are ported once.
Why: Go actions do not scale past a handful of templates and nobody outside the codebase can add one; declarative HTTP steps break down as soon as an action needs a login flow, a CLI or a database query.

## D-030 Agents never see secret values; the server generates random secrets (2026-11-07)
No API token, admin included, ever receives a secret value. Humans enter secret values in the dashboard (an agent asks for one with a request link, like an approval) and see credentials there. Action outputs marked secret are only shown in the dashboard; secret values are masked in any action output or error. Agents never write secret values either: a language model does not produce good randomness and should not handle keys. When an app needs a random secret nobody has to see (a signing key, an internal password), the agent asks sb-control to generate it (`sb env generate`), or the template declares it as generated; the value is created with the server's random number generator and only its name is visible to the agent.

## D-029 Fence a dead server the moment Hetzner reports it off (2026-11-07)
Measured on a live install: after a power-off, Kubernetes marks the node unreachable only after about 47 seconds, and until then the dead server's pods stay in the endpoints, so about half of all requests to the login wall hang. sb-guard now lists the install's servers on every 5-second check and fences a server Hetzner reports off or gone at once: its `sb/role` label changes so the load balancer drops it, and the out-of-service taint marks its pods terminating, which removes them from the endpoints within a second. Replacement starts 30 seconds after the server was first seen off. If the server comes back on before that, the fence is undone. The guard runs as two replicas on different servers with a Lease renewed in the background (shared with sb-control, `internal/lease`), so losing the guard's own server costs about 10 seconds more. Traefik routes to each Service's cluster IP (`nativeLBByDefault`) instead of pod IPs from its own watch, which can stay on a dead server's API server for up to a minute; kube-proxy reads endpoints from the API server on its own server. Load balancer health checks run every 3 seconds with 2 retries. A server that fails while Hetzner still reports it running (network or k3s failure) is still noticed by Kubernetes after about 47 seconds.
