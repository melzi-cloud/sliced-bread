# Hosting modes and TLS

Decided 2026-11-06. Recorded as D-021 and D-022 in [decisions.md](decisions.md).

## Decision 1: self-hosted mode ships first

There are three ways to run SlicedBread:

| Mode | Who runs the control plane | Compute | Status |
|---|---|---|---|
| **Self-hosted** | The user's own cluster | The user's Hetzner account | **First release** (public launch) |
| **Hosted by us** | Us | Our Hetzner account | Later. Start with a waitlist on the launch page to measure demand. |
| **Bring your own servers** | The user | Servers they register with a join script | Later. Needs storage without network volumes and node join without the Hetzner API (see `TODO.md`, future work). |

Why self-hosted first:
- It is what exists. `sb-control` already runs inside each tenant's cluster and the provisioner already builds a tenant from a Hetzner token and a Cloudflare token.
- No billing, no abuse handling on our IPs, no shared Hetzner project limits (50 placement groups, rate limits).
- It matches the open-source, no-lock-in positioning.

### Self-hosted install flow (target)

```
brew install slicedbread        # or a release binary with checksums
sb init
  -> Hetzner API token (recommend a dedicated Hetzner project; tokens are project-wide)
  -> Cloudflare API token (Zone / DNS / Edit on one zone)
  -> base domain, e.g. apps.example.com
  -> size: 1 node (no HA, cheapest) or 3 nodes (HA, recommended)
  -> backup target: Cloudflare R2 (same Cloudflare account) or Hetzner Object Storage (keys pasted)
  -> creates servers, load balancer (3-node), DNS, wildcard certificate, platform
  -> opens the dashboard and runs `sb login`
```

Run from the user's laptop, like `hetzner-k3s` or Kamal. Not `curl | bash` on a server: a server that runs the installer but is not created through the Hetzner API cannot be replaced or moved by us. After install the laptop is not needed: the Hetzner token (node replacement, scaling), Cloudflare token (certificates) and admin users live in the cluster.

Things to check during build:
- Whether Hetzner Object Storage keys can be created through the API (as far as we know they cannot).
- Exact R2 token permissions for bucket creation and S3 credentials.

## Decision 2: TLS depends on the mode

### Self-hosted: TLS ends inside the user's cluster

Unchanged from the current design:
- Apps at `<app>.<base domain>`. One wildcard certificate per tenant, issued in the cluster by cert-manager with Cloudflare DNS-01.
- Private keys never leave the user's cluster. Traffic never passes through us.
- Rate limits are not a concern: each user has their own registered domain.
- **Add a fallback issuer.** Let's Encrypt first, then ZeroSSL (free ACME, needs an external account binding key). Separate rate limits and no single point of failure.

### Hosted by us: we terminate TLS at a shared edge

A common pattern for hosted app platforms (one wildcard certificate on a shared edge, flat app hostnames, a router behind it):
- **Flat hostnames on a user-content domain**: `<app>-<tenant>.slicedbread.cloud` (exact separator to be decided). One label under the domain, so **one wildcard certificate covers every tenant**. No certificate is issued when a tenant signs up or deploys.
- **TLS terminates at our edge**, which then connects to the tenant cluster over TLS.
- Two ways to build the edge:
  - **Cloudflare proxy.** Universal SSL covers `*.slicedbread.cloud` for free (one level only). Cloudflare connects to each tenant's load balancer, which serves a Cloudflare Origin CA certificate (free, trusted only by Cloudflare). Cloudflare Tunnel is an alternative: tenant clusters dial out, so they need no public load balancer at all. That saves the load balancer cost and hides origin IPs; worth checking.
  - **Our own edge** (Traefik or Caddy on a few servers) holding the wildcard, issued with DNS-01. More work, no third party in the path.
- Custom domains in hosted mode: Cloudflare for SaaS (custom hostnames), or ACME HTTP-01 at our own edge. Not designed yet.

Accepted tradeoffs for hosted mode, to state plainly in the hosted terms:
- The edge (we, and Cloudflare if used) sees requests in plaintext.
- Cloudflare's free and Pro plans cap request bodies at 100 MB. That affects large uploads (Vaultwarden attachments, `sb deploy --dir`).
- Streaming video through Cloudflare (Invidious-style apps) needs a check against their terms.

## What the MVP thread should do now so hosted mode stays possible

These cost little now and avoid a rewrite later:

1. **Hostname scheme behind one function.** Mode decides the format: self-hosted `<app>.<domain>`, hosted `<app>-<tenant>.<edge domain>`. Keep every generated label at most 63 characters. Shorten long ones to a prefix plus a short stable hash, and have `sb apps` show the real host so nobody composes it by hand.
2. **Login wall that works with host-only cookies.** In hosted mode there is no tenant-level parent domain to scope a tenant-wide cookie to. Sign in on the auth host, then hand each app host a signed one-time token that it exchanges for its own host-only session cookie. Use the same flow in self-hosted mode: one code path, and better isolation between apps. Together with the cookie hardening in `TODO.md` item 5.
3. **Trust forwarding headers only from the edge.** In hosted mode, Traefik should accept `X-Forwarded-For` and `X-Forwarded-Proto` only from edge IP ranges, and the origin should refuse direct traffic (allow-list or Cloudflare authenticated origin pulls). Hetzner Cloud Firewalls cannot be attached to load balancers, so this has to happen at Traefik or by using a tunnel.
4. **Uploads not limited to 100 MB.** Either keep the API host outside the proxy (DNS only) or make `sb deploy --dir` uploads chunked.
5. **Issuer fallback** (ZeroSSL) as a second cert-manager issuer, with the EAB key stored as a secret.
6. Keep user content on a domain separate from the console (already D-007).

## First release checklist (self-hosted)

The demo flows from the user stories page, plus what launch readers will ask about:
- `sb init` with 1-node and 3-node sizes, and a cost table (1 node vs 3 nodes vs a single VPS running Coolify).
- Backups with nightly restore tests (roadmap M6). The pitch depends on it.
- `sb upgrade` for sb-control, k3s and platform components, with rollback.
- `sb export` (plain Kubernetes manifests) and `sb destroy`, documented as the way out.
- The `TODO.md` "Do now" security items: placement group, metadata service blocked from pods, cookie hardening.
- A short threat model page.
- A 2-minute video: `sb init`, then the three demo prompts.
