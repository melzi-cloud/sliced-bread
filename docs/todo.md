# SlicedBread TODO

Status (2026-11-06, implementation thread): items 1 (placement group), 2 (metadata block, CSI hostNetwork, token rotation) and 3 (cross-app internal URLs) are implemented; 1 and 2 are verified by the smoke test. Item 4 (components and replicas) is open and needs a design decision. Item 5 (cookie hardening) is open. See also [hosting-modes.md](hosting-modes.md).

Written 2026-11-06 against `main` at `3a1af24`, from the user stories review (`/tmp/slicedbread-user-stories/index.html`).
This file lives in `/tmp` and macOS clears `/tmp` on reboot. The implementation thread should copy it into the repo (for example `docs/todo.md`) and keep it there.

## Do now

### 1. Put tenant servers in a spread placement group

**Why:** without one, Hetzner may place all 3 VMs on the same physical host. One hardware failure would then take down all three k3s servers and etcd quorum, which defeats the 3-node HA design.

**What:**
- In `internal/tenant/hetzner.go` `Up()`, create a placement group of type `spread` before the servers, with the usual `sb/run-id`, `sb/owner`, `sb/expires-at` labels, and pass it in `ServerCreateOpts.PlacementGroup`.
- Add `placement_group` to `Inventory()` and `deleteAll()` (delete after servers, before the network), so teardown and the reaper remove it.

**Hetzner limits to design around** (from docs.hetzner.com/cloud/placement-groups/overview):
- Up to 10 servers per spread group. Tenants larger than 10 nodes need a second group.
- One placement group per server.
- **Up to 50 placement groups per project.** If all tenants share one Hetzner project, that caps us at 50 tenants with spread placement. Same question as the per-project server limit and API rate limit: decide how tenants map to Hetzner projects.

**Done when:** `make e2e-hetzner` passes, the smoke test asserts all servers share one spread group, and nothing labelled remains afterwards (including the group).

### 2. Stop pods from reading Hetzner metadata (user data)

**Why:** `http://169.254.169.254/hetzner/v1/userdata` is reachable from every pod. Our user data contains the **k3s join token** and each node's **SSH host private key**. With the token, any compromised app pod can join the cluster as a server node (the API on node IPs is reachable from pods) and take over the tenant. With the host key, it can impersonate a node to SSH clients.

**What, in layers:**
1. **Block pod traffic to the metadata IP at the node.** An nftables rule installed by cloud-init that drops traffic from the pod CIDR (`10.42.0.0/16`) to `169.254.169.254`. This covers every pod in every namespace, including ones without a NetworkPolicy. Host-network processes (cloud-init, k3s itself) are unaffected.
2. **Check the Hetzner CSI node plugin first.** Its DaemonSet runs with `hostNetwork: false` by default and likely reads metadata to find its server ID. Set `node.hostNetwork: true` in the `hcloud-csi` chart values so its requests come from the node, not the pod CIDR. Confirm the volume check in the smoke test still passes.
3. **Rotate the k3s token once the cluster has formed** (`k3s token rotate`), so the value left in user data is useless even if it leaks some other way. Later joins (node replacement, scale-up) get a fresh token at that time.
4. Optionally also block `169.254.169.254` with an egress rule in the per-app NetworkPolicy, as defence in depth.

**Done when:**
- From a pod in an app namespace, `wget -T 3 -qO- http://169.254.169.254/hetzner/v1/userdata` fails.
- The CSI volume check in `make e2e-hetzner` still passes.
- The token in user data no longer joins the cluster.

Add both checks to the smoke test so this cannot regress.

### 3. Fix cross-app internal URLs (bug)

**Why:** `${apps.<other>.internal}` resolves to `http://web.app-<other>.svc.cluster.local`, but the other app's `sb-isolation` NetworkPolicy only accepts traffic from its own namespace, `kube-system`, `sb-system` and `cnpg-system`. The call is blocked, even though the skill documents it for API backends.

**What:** when app A references `${apps.B.internal}`, add an ingress rule to B's policy allowing namespace `app-A` (or a label-based allow list). Removing the reference removes the rule.

**Done when:** a test deploys two apps where one calls the other through `${apps.x.internal}` and gets a response, and a third, unrelated app is still blocked.

### 4. Replicas and several components per app

**Why:** replicas are hardcoded to 1 in `internal/apps/render.go`, and an app is one container. The Invidious story (story 6 in the user stories page) needs, in one namespace:
- several components, each with a Service named after itself
- replicas per component (3 companions)
- config files mounted into containers, with secret and database references
- a route table sending paths to different components

The Coolify and Dokploy templates we planned to adapt are multi-service compose files, so the same model change is needed for the catalog.

**What:** a design decision first. The proposed `slicedbread.yaml` in story 6 is a starting point. For replicas > 1, also add topology spread across nodes and a PodDisruptionBudget. Refuse replicas > 1 on a component with a single-attach volume.

### 5. Harden login wall cookies

**Why:** until `slicedbread.cloud` is on the Public Suffix List (months away, and only likely once there are thousands of users), any tenant subdomain can set cookies for the whole `slicedbread.cloud` domain. The signed session cannot be forged, but another tenant can shadow it (log users out) or break CSRF protection that relies on a double-submit cookie. Hosted mode needs host-only cookies anyway (see [hosting-modes.md](hosting-modes.md)).

**What:**
- `__Host-` prefix on cookies set by `auth.<tenant>` (host-only, cannot be set from another subdomain).
- Per-tenant cookie names (for example `sb_s_<tenant-id>`) so they cannot be shadowed by name.
- CSRF tokens tied to the session, not a separate cookie.
- Session signature covers the tenant id.
- Move toward the per-host session exchange described in [hosting-modes.md](hosting-modes.md).

**Done when:** a test sets a conflicting `sb_session` / `sb_csrf` cookie for the parent domain and the wall and approvals still work correctly.

## Future work

Look into these later. None block the current demo.

### Database replication
- CNPG with `instances: 2` (primary and replica on different nodes, anti-affinity) for `--database` apps and template databases. Today both run a single instance, so a node loss means downtime until Postgres restarts elsewhere with its volume.
- Option for synchronous replication where losing the last few transactions is unacceptable.
- WAL archiving and base backups to object storage (Garage in dev) for point-in-time recovery. This also enables "restore into a copy" (story 10).
- On bare metal this is the main HA mechanism, because databases replicate themselves on local disk without a network volume.

### File volume backup, restore and move (one engine)
- One code path for: scheduled backups, nightly restore tests, planned moves (drain a node), and recovery after node loss.
- Planned move: stop the app, final incremental backup, restore on the target node, start. Downtime is the copy time.
- Unplanned: restore from the latest backup. How much data is lost depends on how often backups run; restic or kopia incrementals every 5 to 15 minutes are cheap.
- Put volume handling behind an interface: `network-volume` (Hetzner CSI, reattach elsewhere) and `backup-restore` (local disk, move by backup and restore).
- Evaluate before building: VolSync (scheduled restic/rsync replication of volumes), Velero. Avoid Longhorn (deadlocks in practice).
- Hetzner volumes have no snapshots as far as we know, so backups must be file-level or app-level either way.

### Track real resource usage so scaling works
- Today every app requests `50m` CPU and `64Mi` memory with a `1Gi` memory limit. The scheduler believes apps are tiny and overpacks nodes. The result is memory pressure and OOM kills, not unschedulable pods, so any "add a node when pods are pending" rule never fires.
- Collect per-app CPU and memory over time (k3s ships metrics-server; keep a rolling window, for example p95 over 7 days).
- Set requests from observed usage (a VPA-style recommender). Templates declare known defaults; user apps start from a sensible default and converge.
- Count volume slots as capacity too (Hetzner allows 16 volumes per server).
- Limit concurrent builds, or run builds on a separate node, since BuildKit competes with apps.

### Autoscaling and spend caps
- Admission check at deploy time: "fits", "needs one more server (about EUR X/month)", or "cap reached", answered up front, not left as a Pending pod.
- Scale up when pods are unschedulable for a few minutes or requested resources stay above about 80%. Scale down only after hours of low use, draining carefully.
- Our own logic, not cluster-autoscaler: we already create and join nodes, and we need caps and approvals.
- Customer sets a monthly spend or node cap. Growth inside the cap is automatic with a notification. Going over the cap is an approval request, the same way exposure changes are today (exit code 3). At the cap, new deploys are refused with the reason and running apps keep running.

### Hetzner project layout
- Per-project limits apply to all tenants in a project: server count, 50 placement groups, API rate limit. Decide on one project for all tenants, several pools, or one per customer. Check whether projects can be created through the API (as far as we know, they cannot).

### Non-Hetzner nodes (todo, not now)
- Customer hardware or other providers. Needs: a storage strategy without network volumes (see the two items above), a replacement for the Hetzner load balancer, and node join without the Hetzner API.
