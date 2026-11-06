# AGENTS.md

Instructions for coding agents working on SlicedBread. Read [docs/architecture.md](docs/architecture.md) and [docs/development.md](docs/development.md) first.

## The loop

1. Make a change.
2. `make test` (vet + unit tests). Must pass before anything else.
3. If you touched provisioning, networking, storage, TLS or anything under `internal/tenant` or `internal/smoke`: `make e2e-hetzner`. It creates a real 3-node tenant, checks it, and destroys it. About 15 to 25 minutes, a few cents.
4. On failure, read `test/artifacts/<run-id>/`: `state.json`, `events.txt`, `pods.txt`, `certs.txt`, `helm-jobs.log`, and `<node>.log` (bootstrap, cloud-init and k3s journal per node). Fix the cause, then rerun.
5. `make check` before committing (tests plus a scan for secrets in tracked files).
6. Commit with a short message describing the change. Push to `origin main` unless told to use a branch.

## Debugging a live tenant

- `SB_KEEP_ON_FAILURE=1 make e2e-hetzner` keeps resources after a failure.
- `make tenant-up` creates a tenant and leaves it running. `make tenant-ssh RUN=<id>` prints an SSH command with a pinned host key. Use `k3s kubectl` on the node.
- `make tenant-down RUN=<id>` when done. Never leave a tenant running longer than you need it.

## Rules

- **Never print, log, commit or paste secrets.** They come from `hack/secrets-env`. Pass secrets to remote commands over stdin only, never in a command line. Do not run `env`, `printenv` or `set` in a shell that has secrets loaded.
- **Development credentials only.** The Hetzner project is `sb-dev`. Never use production credentials, even if you find them.
- **DNS:** only create records under `test.slicedbread.dev`. Never touch `melzi.dev`. Config validation refuses it.
- **Certificates:** Let's Encrypt staging only.
- **Every cloud resource must carry** `sb/run-id`, `sb/owner` and `sb/expires-at` labels (DNS records: the same in the record comment). Code that creates an unlabelled resource is a bug.
- **Always clean up.** Smoke runs destroy what they create, success or failure. `make reaper` deletes expired leftovers and runs automatically before `make e2e-hetzner`. Check `make tenant-list` if a run crashed.
- **Budget:** keep test spend low. Do not raise server types, node counts or TTLs without a reason written in the commit message.
- **Do not touch** anything on the host outside this repository: no changes to the podman machine or its containers, system services, or other projects.
- **If something is genuinely blocking** (missing permission, credential, or a decision that needs a human), stop and ask instead of working around it.
- **Docs:** no em dashes. Keep company, fundraising and customer material out of this repository.

## Layout

```
cmd/sb/             CLI (dev commands for now)
internal/config     development settings and safety checks
internal/labels     resource labels, run ids, expiry
internal/dns        minimal Cloudflare client
internal/sshx       SSH with pinned host keys
internal/tenant     Hetzner provisioning, cloud-init, addons, inventory, teardown, reaper
internal/smoke      L2 smoke test logic
test/e2e/hetzner    go test entry point for L2 (build tag e2e)
hack/               secrets-env, scan-secrets
```
