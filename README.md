# SlicedBread

Making self-hosting just a bit easier.

---

I've been self-hosting for 7 years. Over the years, I've gone from nginx static sites -> to manual GNU screen background processes -> Docker -> Compose -> Kubernetes. It has always annoyed me how much work it was to get things set up, and then maintain it.

This project is an attempt at an automated opinionated setup that "just works".

## What it does right now

Takes a Hetzner project and automatically spins up a lightweight Kubernetes cluster (k3s) with some niceties built in:

- Automatic backups (and automatic restore tests to make sure they work)
- Automatic security updates & restart with minimal downtime
- Wildcard DNS certificate & DNS record via Cloudflare
- Configurable global auth wall
- Deployment templates for open source software
- A CLI & skill files for agents to manage deployments

Requirements: Hetzner account, Domain in Cloudflare

Optional: S3 Bucket (anywhere)

[Jump to quickstart](#quickstart)

## WIP

- Don't require Hetzner. Allow users to provide list of hosts & ssh key.
- Allow other DNS providers via something like RFC 2136
- Database replication
- Automatic image updates
- Track resource usage and autoscale with cap.
- Rollback bad OS update
- Automatic pull and build images from GitHub (CI)

## A bit more implementation detail & why

### Kubernetes (k3s)

Your first reaction might be "wtf, complexity, go away". Mine too. But every setup I've run
eventually grew a worse, hand-rolled version of the same features:

- **Where does this go?** With 5 machines, I shouldn't be the one checking which has room.
- **Does it come back after a reboot?** No more writing a systemd unit per service.
- **Where is its config?** Not scattered across `/etc`, a compose file and some anonymous volume.
- **What if a machine dies?** Its workloads move to the others.
- **What if it needs more?** Stateless things replicate and load-balance across machines.

And everything else: Health checks, self-healing, resource limits, and draining a server before a restart.

### Authentication

Annoyingly, a bunch of very useful things don't come with auth. This just allows you to configure a global authentication wall without putting a bunch of basic auth entries and hashes in your Caddy config.

### Backups

Volume, config, environment, and everything else goes in an encrypted backup on S3. Backups are meant to be off-site and reliable, and so they're encrypted so you can use an untrusted provider rather than try to self-host minio elsewhere.

Backups are actually tested with restores after they finish on a temporary namespace to make sure they're working. A backup untested is barely better than not having one.

### Automatic updates

No more `ssh` -> `sudo apt upgrade`. This is another reason for Kubernetes. We can just upgrade and restart (when kernel updates) without worrying about how long it takes to come back up. The apps and volumes would've already moved to another node.

### Templates

Not a lot of them for now, but the goal is to be able to reuse the efforts of others so getting something hosted becomes a click rather than a process.

### CLI

For AI agents. They're limited from destructive or dangerous actions like deleting resources, making things public, etc. You get a link to approve them from the dashboard.

## Quickstart

[TODO]
