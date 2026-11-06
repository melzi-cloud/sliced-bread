# Roadmap

The first goal is a working demo of three flows:

1. "Here's my Lovable export" or "connect this GitHub repo": it is detected, built and running behind the login wall.
2. "Deploy Vaultwarden": running in `app-auth` mode with hardening and backups.
3. "Add analytics to my app with Umami", driven by the user's own coding agent through the MCP server.

## Milestones

| | Milestone | Done when |
|---|---|---|
| M0 | One tenant created by a script | 3 Debian VMs, k3s HA, Hetzner CSI, Traefik, cert-manager with a wildcard cert, Hetzner LB, firewall allowing only 80/443. **Done 2026-11-06** (`make e2e-hetzner`) |
| M1 | Minimal control plane | GitHub login, org = tenant, API, desired state, tenant agent pulling and applying |
| M2 | Deploys | Container image and GitHub repo (Railpack) deploy to `<app>.<tenant>.slicedbread.cloud` with TLS |
| M3 | Login wall | Forward-auth, cookie stripping, access modes, public paths |
| M4 | Templates | Vaultwarden and Umami pass the verification workflow |
| M5 | Agent interface | MCP server, CLI, `AGENTS.md`. Demo 3 works end to end with Claude Code or Cursor |
| M6 | Recovery | Nightly backups, nightly restore tests with alerts, node replacement |

M6 is required before any real data, including our own Vaultwarden, moves onto the platform.

## Acceptance tests

1. Hard-kill a node: every app is back within the recovery target with no data loss.
2. Delete an app's namespace: one command restores it from backup.
3. Corrupt a Postgres table: point-in-time recovery brings it back.
4. Patch day: OS updates and reboots happen with no manual steps and no user-visible downtime.
5. A bad image update rolls back automatically.
6. Without a session, every `wall` app returns the login wall. Only declared public paths bypass it. A port scan of the tenant shows only 80/443.
7. A compromised app cannot read the session cookie or reach another app's database.
8. Starting from "add analytics to this app", an agent ends with a working Umami receiving events.
9. Restore tests run nightly and a failure raises an alert.

## Later

- Customer Hetzner accounts (same code, different API token)
- Tailnet access for SSH, databases and non-HTTP apps
- More identity providers, custom domains
- Built-in agent for background operations
- Lovable Cloud backend migration helper
- Shared-pool runtime for short-lived jobs
