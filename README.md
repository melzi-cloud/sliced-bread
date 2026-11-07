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

bin/sb init --domain apps.example.com --size 1 \
  --backup-endpoint https://<account>.r2.cloudflarestorage.com --backup-bucket my-backups
```

`sb init` runs from your machine and takes 5 to 10 minutes. It creates the servers, firewall, DNS records, a wildcard certificate from Let's Encrypt and the control plane. At the end it prints:

1. **A one-time setup link.** Open it and create your owner account (email and password). Your email also sets up ZeroSSL as a fallback certificate authority. If the link expires, `sb setup-link <install>` prints a new one.
2. **The backup encryption password.** Save it in your password manager. Backups are encrypted with it, and if the servers are ever lost it is the only way to read them. It is not stored on your machine.

**Humans use the dashboard, agents use the CLI.** The dashboard at `https://auth.apps.example.com` is where you approve what agents ask for, invite teammates, set up backups, restore, configure alerts and export. The CLI is left logged in with an agent token: deploys and configuration go through, but making apps public, deleting and restoring wait for your approval, so you and your coding agent can safely share it.

Then:

```sh
bin/sb deploy vault --template vaultwarden && bin/sb wait vault
bin/sb deploy myapp --dir ~/code/my-lovable-export && bin/sb wait myapp
bin/sb deploy analytics --template umami && bin/sb wait analytics
bin/sb action analytics create_website --input name=MyApp --input domain=myapp.apps.example.com
bin/sb backups
```

To let a coding agent operate it, give it [skill/SKILL.md](skill/SKILL.md). On the machine that ran `sb init` it can use the CLI as is; elsewhere it runs `sb login --api https://api.apps.example.com` and you approve that login in the dashboard. No CLI token can approve, change settings or see secrets. For automation such as CI, an admin can create an expiring admin token on the dashboard's Settings page.

**Losing the whole install.** Every app is backed up, including apps without data (their snapshot holds settings, secret values and uploaded source). To rebuild elsewhere, run `sb init` with `SB_BACKUP_PASSWORD` set to the saved password and the same backup bucket and prefix, finish setup, then on the dashboard's Settings page use "Look in the backup repository" and "Recreate all". Use the same domain if you can: references like `${apps.analytics.url}` are recomputed, but URLs written into an app's own source (for example a tracking snippet) still point at the old domain until the app is redeployed with new ones.

Other commands: `sb installs`, `sb upgrade <install>`, `sb setup-link <install>`, `sb ssh <install>` (SSH is only open to the IP address `sb init` ran from), `sb destroy <install>`.

## Sizes and cost

Hetzner prices in nbg1 including VAT, checked 2026-11-06 (cx23: 2 vCPU, 4 GB RAM, 40 GB disk).

| Size | What you get | About |
|---|---|---|
| `--size 1` | 1 server, ports 80/443 open on it. App data on Hetzner volumes (they survive the server, but nothing moves them). No high availability: if the server fails, apps are down until it is back or restored from backup, and the nightly OS reboot takes apps down for a minute or two. | EUR 7/month |
| `--size 3` | 3 servers in a spread placement group (different physical hosts), a load balancer, embedded etcd that survives losing one server, two copies of the login wall. A dead server is replaced automatically (in the node-loss test, apps were serving again within 5 minutes and a new server had joined after about 3); OS reboots happen one server at a time without downtime for apps without a volume. | EUR 29/month |
| per app with data | each volume or database is a 10 GB Hetzner volume | EUR 0.77/month |

Backup storage is billed by your S3 provider. Restic deduplicates and compresses, so small apps use little space.

## What it does today

- **Deploy anything:** a catalog template (Vaultwarden, Umami), any container image, a public git repo, or a local directory such as a Lovable export, built in the cluster with Railpack or its Dockerfile.
- **Your team, your rules:** invite teammates as members or admins on the dashboard. Members get past the login wall; whether they can also approve risky changes is an admin setting (off by default).
- **Private by default:** every app sits behind a login wall unless you say otherwise. Apps with their own clients (Vaultwarden) opt out. Session cookies are host-only and never reach apps.
- **Backups that are proven:** daily restic backups of volumes and Postgres to your bucket, each followed by a restore test into a scratch namespace: restic verifies every restored file's content, the file count must match, required files must be present (Vaultwarden's database), and a database dump must load into a fresh Postgres with the same tables. `sb restore` replaces an app's data with a snapshot, and `sb restore <app> --recreate` brings back a deleted app (spec, secrets, source and data) from the bucket alone.
- **Tells you when something breaks:** failed backups or restore tests, apps that stay down, certificates that don't renew and servers that disappear show on the dashboard and in `sb alerts`, and can be posted to a webhook (Slack, Discord, ntfy or JSON) when they open and when they clear.
- **Maintains itself:** Debian security updates install automatically. Servers that need a reboot are restarted one at a time in a nightly window, after their apps have moved: apps without a volume keep serving, apps with a volume pause for under a minute. Newer upstream versions of template images are reported (not applied). On 3 servers, a server that dies is fenced and replaced by a new one with no manual steps.
- **Agent-friendly:** a CLI with JSON output and documented exit codes, a skill file, and human approvals for anything that widens exposure, deletes or restores.
- **Your account, your keys:** certificates are issued and kept inside your cluster, and traffic never passes through us. The dashboard exports every app as plain Kubernetes manifests.

## Known gaps

- Image updates are reported, not applied; no automatic rollback of a bad update yet.
- Apps with a volume or database (one Postgres instance per app) pause for about a minute when their server reboots or dies, while the volume moves.
- Kubernetes (k3s) itself is not upgraded automatically yet.
- Alerts go to the dashboard, `sb alerts` and one webhook (JSON, Slack, Discord or ntfy). No email yet.
- Apps run as root inside their containers (capabilities reduced, privilege escalation off).
- The in-cluster image registry has no login. App pods cannot reach it (network policy) and apps run images by digest, but build pods can push to it.
- Login is email and password. Teammates join through invite links you send yourself (no email sending, no GitHub sign-in yet).
- A restore test checks that Vaultwarden's SQLite file is present, not its internal integrity.
- Hetzner and Cloudflare only.

## Recovery, measured

`make e2e-node-loss` powers off, through the Hetzner API, the server running Vaultwarden on a 3-server install with Vaultwarden and Umami in use, and measures until each app serves again. In the latest run (2026-11-06) that server also ran Umami's database, the database operator and one of the two control-plane replicas:

| | Back after |
|---|---|
| Dashboard and login wall | 2 min 3 s |
| Replacement server joined | 3 min 9 s |
| Vaultwarden | 4 min 5 s |
| Umami (through its database) | 4 min 59 s |

No data was lost, including a Vaultwarden account created seconds before the power-off. Most of the time is spent moving Hetzner volumes to another server.

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
