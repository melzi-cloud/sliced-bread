# Development

How we build and test SlicedBread so that coding agents can verify their own changes safely.

## Principles

1. An agent must be able to answer "did my change work?" without a human.
2. Most work is verified locally in minutes. Real cloud tests exist for the parts only real infrastructure can prove.
3. Agents only ever hold development credentials, every resource they create expires, and nothing a test does can touch production.

## Environments

| What | Development / test |
|---|---|
| Hetzner | Project `sb-dev`, with low resource limits |
| DNS | Cloudflare zone `slicedbread.dev`, test hosts under `test.slicedbread.dev` |
| Hostnames | `<app>.<run-id>.test.slicedbread.dev` |
| Certificates | Let's Encrypt **staging** |
| Backups | Test bucket with automatic expiry |

Production gets its own Hetzner project and credentials later. Test tooling must never read production credentials.

## Secrets

- Stored in a dedicated macOS keychain, `~/Library/Keychains/slicedbread-dev.keychain-db`, separate from the login keychain. It has no auto-lock and must be unlocked once after each reboot.
- Item names: `sb-dev-hcloud`, `sb-dev-cloudflare`, `sb-dev-s3-key`, `sb-dev-s3-secret`.
- `hack/secrets-env` loads them into the environment of a single command. Scripts never print secret values, and never write them to files or logs.
- If the keychain is locked, scripts stop with a clear message.
- Never paste secrets into an agent chat. Chat text is sent to the model as written.
- CI uses GitHub Actions secrets.

Token scopes:

- Hetzner: read and write, project `sb-dev` only. Rotate by hand.
- Cloudflare: Zone / DNS / Edit on `slicedbread.dev` only, with an expiry date.
- Object storage: limited to the test bucket where the provider allows it.

## Test ladder

| Level | Command | Runs on | Covers | Time |
|---|---|---|---|---|
| L0 | `make test` | anywhere | rendering, access modes, template validation | seconds |
| L1 | `make e2e-local` | k3d | reconciler, login wall, cookie stripping, templates, MCP tools | minutes |
| L2 | `make e2e-hetzner` | throwaway Hetzner tenant | CSI volumes, load balancer, firewall, DNS, wildcard certs, node kill, restore | about 10 to 15 minutes |
| L3 | `make acceptance` | Hetzner | all acceptance tests in [roadmap.md](roadmap.md) | about an hour, nightly |

When to run what:

- Every change: L0 and L1.
- Changes to provisioning, networking, storage or recovery: L2 as well.
- Nightly: L3.

Every failed e2e run writes cluster state, events and logs to `test/artifacts/<run-id>/` so the agent can debug from them.

## Guardrails against cost and leftovers

- Every cloud resource gets the labels `sb/run-id`, `sb/owner` and `sb/expires-at`.
- Default lifetime is 2 hours. L3 runs may request longer.
- `hack/reaper` deletes expired resources and runs every 15 minutes.
- Low project limits (servers, volumes, load balancers) act as the hard cap, together with the reaper.
- Every run destroys what it created on exit, success or failure. The reaper is the backstop.

## Repository layout

```
cmd/        sb (CLI), sb-control, sb-agent, sb-authproxy, sb-mcp
internal/   provision, render, reconcile, auth, templates
templates/  vaultwarden/, umami/
test/       e2e/local, e2e/hetzner, acceptance, artifacts/ (gitignored)
hack/       secrets-env, tenant-up, tenant-down, reaper
AGENTS.md   Makefile
```

Language: Go. Main dependencies: hcloud-go, Hetzner CSI driver and cloud controller manager, k3s, k3d, Traefik, cert-manager, CloudNativePG, kured, the official MCP Go SDK (check its license file before release).

## Order of work

1. Repo skeleton, Makefile targets, `AGENTS.md`, local k3d environment, CI for L0 and L1.
2. Provisioner (`sb dev tenant up/down`), reaper, and the first L2 smoke test: create a tenant, deploy `whoami` behind TLS at `whoami.<run-id>.test.slicedbread.dev`, check it responds, destroy everything.
3. Acceptance tests written as failing specs, then milestones M1 to M5 against them.
