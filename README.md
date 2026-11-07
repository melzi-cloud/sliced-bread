# SlicedBread

Run off-the-shelf software and AI-built apps on your own Hetzner servers, private by default, with backups that are restored and checked after every run. Built to be operated by the coding agent you already use.

> Status: pre-release. Works end to end on Hetzner Cloud with a Cloudflare-managed domain. Not production ready: read the [threat model](docs/threat-model.md) and the known gaps below first.

## Quickstart

You need a Hetzner Cloud project, a domain on Cloudflare, and optionally an S3-compatible bucket for backups (Cloudflare R2, Hetzner Object Storage, or any S3 service), ideally in a different location from your servers.

```sh
make build                                    # in a clone of this repository: bin/sb, with the control plane built in

export HCLOUD_TOKEN=...                       # Hetzner: read and write token. Use a dedicated project.
export CLOUDFLARE_API_TOKEN=...               # Cloudflare: Zone / DNS / Edit on the zone below (CF_API_TOKEN also works)
export SB_BACKUP_ACCESS_KEY_ID=...            # optional: S3 keys for backups
export SB_BACKUP_SECRET_ACCESS_KEY=...

bin/sb init --domain apps.example.com --email you@example.com --size 1 \
  --backup-endpoint https://<account>.r2.cloudflarestorage.com --backup-bucket my-backups
```

`sb init` runs from your machine and takes 5 to 10 minutes. It creates the servers, firewall, DNS records, a wildcard certificate from Let's Encrypt (add `--fallback-issuer zerossl` to fall back to ZeroSSL, which uses your `--email`) and the control plane, then logs the CLI in with an admin token. It also creates an agent token (`cli-agent.json` in the install directory) for coding agents: with it, exposure changes, deletes and restores wait for your approval. It prints where it saved the dashboard login and the backup encryption password. Keep the password somewhere safe: without it the backups cannot be read.

Then:

```sh
bin/sb deploy vault --template vaultwarden && bin/sb wait vault
bin/sb deploy myapp --dir ~/code/my-lovable-export && bin/sb wait myapp
bin/sb deploy analytics --template umami && bin/sb wait analytics
bin/sb action analytics create_website --input name=MyApp --input domain=myapp.apps.example.com
bin/sb backups
```

To let a coding agent operate it, give it [skill/SKILL.md](skill/SKILL.md) and have it run `sb login --api https://api.apps.example.com`. You approve that login in the browser, and the agent gets a token that cannot widen access, delete apps or restore data without your approval.

Other commands: `sb installs`, `sb upgrade <install>`, `sb ssh <install>` (SSH is only open to the IP address `sb init` ran from), `sb export <dir>`, `sb destroy <install>`.

## Sizes and cost

Hetzner prices in nbg1 including VAT, checked 2026-11-06 (cx23: 2 vCPU, 4 GB RAM, 40 GB disk).

| Size | What you get | About |
|---|---|---|
| `--size 1` | 1 server, ports 80/443 open on it. App data on Hetzner volumes (they survive the server, but nothing moves them). No high availability: if the server fails, apps are down until it is back or restored from backup. | EUR 7/month |
| `--size 3` | 3 servers in a spread placement group (different physical hosts), a load balancer, embedded etcd that survives losing one server. App data on Hetzner volumes that can move to another server. | EUR 29/month |
| per app with data | each volume or database is a 10 GB Hetzner volume | EUR 0.77/month |

Backup storage is billed by your S3 provider. Restic deduplicates and compresses, so small apps use little space.

## What it does today

- **Deploy anything:** a catalog template (Vaultwarden, Umami), any container image, a public git repo, or a local directory such as a Lovable export, built in the cluster with Railpack or its Dockerfile.
- **Private by default:** every app sits behind a login wall unless you say otherwise. Apps with their own clients (Vaultwarden) opt out. Session cookies are host-only and never reach apps.
- **Backups that are proven:** daily restic backups of volumes and Postgres to your bucket, each followed by a restore test into a scratch namespace: restic verifies every restored file's content, the file count must match, required files must be present (Vaultwarden's database), and a database dump must load into a fresh Postgres with the same tables. `sb restore` replaces an app's data with a snapshot.
- **Agent-friendly:** a CLI with JSON output and documented exit codes, a skill file, and human approvals for anything that widens exposure, deletes or restores.
- **Your account, your keys:** certificates are issued and kept inside your cluster, and traffic never passes through us. `sb export` writes every app as plain Kubernetes manifests.

## Known gaps

- No automatic OS patching, node replacement or rollback of bad image updates yet.
- No alerting: a failed backup or restore test shows in `sb backups` and `sb status`, but nothing notifies you.
- Apps run as root inside their containers (capabilities reduced, privilege escalation off).
- The in-cluster image registry has no login. App pods cannot reach it (network policy) and apps run images by digest, but build pods can push to it.
- One Postgres instance per app (no replica). On `--size 3` it moves with its volume when a server fails; on `--size 1` it is down with the server.
- Login is email and password for one admin account. No GitHub login or team members yet, so "private" means only you.
- A restore test checks that Vaultwarden's SQLite file is present, not its internal integrity.
- Hetzner and Cloudflare only.

## Docs

- [Threat model](docs/threat-model.md)
- [Architecture](docs/architecture.md)
- [Hosting modes and TLS](docs/hosting-modes.md)
- [App spec](docs/app-spec.md)
- [Technical decisions](docs/decisions.md)
- [Roadmap and acceptance tests](docs/roadmap.md)
- [TODO](docs/todo.md)
- [Development and testing](docs/development.md)
- [Agent skill file](skill/SKILL.md)

## License

[AGPL-3.0](LICENSE)
