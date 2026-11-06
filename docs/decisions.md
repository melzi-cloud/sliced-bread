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
