# Threat model

What SlicedBread protects against, what it does not, and where the trust boundaries are. Applies to self-hosted installs (D-021). Last reviewed 2026-11-06.

## Assets

| Asset | Where it lives |
|---|---|
| App data | Hetzner volumes and Postgres in the install's cluster; backups in the user's S3 bucket |
| Hetzner and Cloudflare tokens | Secrets in the cluster (`sb-system/sb-provider`, `kube-system/hcloud`, `cert-manager/cloudflare-api-token`) |
| App secret values in backups | Every snapshot carries the app's spec and secret values (and uploaded source), so it can recreate the app; encrypted by restic like the data |
| Backup keys and encryption password | Secret `sb-system/sb-backup`, copied as `sb-backup-env` into namespaces that run backup jobs; the password is also shown once to the admin |
| TLS private keys | Secrets in the cluster; never leave it |
| API tokens | Agent tokens on the user's machine (`~/.config/slicedbread`); expiring admin tokens only where an admin put them for automation; hashes in `sb-system/sb-auth` |
| Dashboard accounts | Owner created through a one-time setup link (code hash in `sb-system/sb-setup`, deleted after use); members through invite links; bcrypt hashes in `sb-system/sb-auth` |
| Login sessions | Signed, host-only browser cookies |

## Trust boundaries

1. **Internet to the install.** Only 80 and 443 are reachable (3 nodes: through the load balancer; 1 node: on the server). SSH is open only to the IP address `sb init` ran from. The Kubernetes API and kubelet are not reachable from the internet (checked by the smoke test).
2. **Browser to apps.** `wall` apps require a session. The auth host issues a one-time exchange token per app host; each app host gets its own `__Host-` cookie that other hosts, including sibling subdomains and other installs under the same parent domain, cannot set, read or shadow. SlicedBread cookies are removed before requests reach apps.
3. **App to app.** Each app has its own namespace with a NetworkPolicy that only admits traffic from the same app, the platform and apps that explicitly reference it with `${apps.<name>.internal}`. App pods get no service account token.
4. **Builds.** Source builds run in `sb-builds`, one pod each, rootless, unreachable from the network. The registry admits only build pods and the nodes; apps run images by digest.
5. **App to node.** Pods cannot reach the Hetzner metadata service (it holds the original join token and host keys); the k3s join token is rotated once the cluster has formed. Containers run with privilege escalation off and a reduced capability set.
6. **Agent to install.** Agent tokens can deploy and configure, but widening access, opening Vaultwarden signups, deleting apps and restoring data need a human approval in the browser: by an admin, or by a member when an admin has allowed that. A solo admin approves their own agent. Removing a member ends their sessions at once and revokes the agent tokens they approved. Approvals go stale when the app changes after the request.
7. **Install to backup bucket.** Backups are encrypted by restic before upload. The bucket provider sees sizes and timing, not contents.

## In scope

- Opportunistic scanning and exploitation of exposed internal tools (the main reason for private-by-default).
- A compromised or malicious app trying to reach other apps, steal sessions, or take over the cluster through metadata.
- A coding agent making a destructive or exposing change the user did not intend.
- Losing a server (3 nodes), losing app data by mistake (`sb restore`), and backups that silently stopped working (restore test after every backup).

## Out of scope or not yet handled

- **A compromised app escaping its container to the node.** Apps share nodes and run as root inside their containers. A kernel or container runtime escape gives access to the node, its secrets and other apps. Run only software you would run on a server you own.
- **Theft of the user's machine or tokens.** The CLI only holds an agent token, but the install folder holds the SSH key for the servers, and the Hetzner token gives full control of the project. Use a dedicated Hetzner project. Admin API tokens exist only if an admin creates one for automation, and they expire within 90 days.
- **The image registry inside the cluster** has no login. A NetworkPolicy (kept in step with the node list by sb-control) lets in only build pods and the nodes' container runtime; app pods cannot reach it. Apps built from source deploy by digest, so overwriting a tag does not change what runs, and deletes through the registry API are off. A malicious build can still push to the registry (but not change a running app's digest).
- **Builds run untrusted code.** Each build is its own Job with its own rootless BuildKit (uid 1000, not privileged, no capabilities to the node), so builds are separated from each other and from the node by the container boundary and a user namespace. They need unconfined seccomp and AppArmor to create that user namespace, so `sb-builds` is a privileged Pod Security namespace that only sb-control creates pods in. A kernel exploit from a build step reaches the node.
- **Malicious template images.** Templates pin upstream images by tag, not by digest.
- **Denial of service.** No rate limiting beyond what Traefik and Hetzner provide.
- **Losing the whole Hetzner location or account.** Backups survive if the bucket is elsewhere, but restoring into a new install is a manual process today.
- **Unpatched OS and images.** Automatic patching and image updates are not built yet.
- **Alerting reaches you only through the dashboard, `sb alerts` and a webhook you configure.** If the whole install is down, nothing inside it can send an alert; an outside uptime check is still needed for that.

## Reporting

Report security issues privately to the maintainers before opening a public issue.
