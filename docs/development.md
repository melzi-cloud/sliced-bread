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
- Item names: `sb-dev-hcloud`, `sb-dev-cloudflare` (account-owned API token), `sb-dev-cloudflare-id` (Cloudflare account ID), `sb-dev-s3-key`, `sb-dev-s3-secret`, `sb-dev-github` (fine-grained token for the repo).
- Non-secret settings (S3 endpoint, bucket, region, DNS zone) live in a gitignored `dev.env` at the repo root.
- Cloudflare account-owned tokens are verified at `/accounts/{account_id}/tokens/verify`, not `/user/tokens/verify`.
- Git pushes use a one-off `GIT_ASKPASS` helper reading `sb-dev-github` with `credential.helper` disabled, so the token never lands in the remote URL, git config or another keychain.
- `hack/secrets-env` loads them into the environment of a single command. Scripts never print secret values, and never write them to files or logs.
- If the keychain is locked, scripts stop with a clear message.
- Never paste secrets into an agent chat. Chat text is sent to the model as written.
- CI uses GitHub Actions secrets.

Token scopes:

- Hetzner: read and write, project `sb-dev` only. Rotate by hand.
- Cloudflare: Zone / DNS / Edit on `slicedbread.dev` only, with an expiry date.
- Object storage: limited to the test bucket where the provider allows it.

## Test ladder

| Level | Command | Runs on | Covers | Time | Status |
|---|---|---|---|---|---|
| L0 | `make test` | anywhere, and CI on every push | config safety checks, labels, DNS client, cloud-init and manifest rendering | seconds | working |
| L1 | `make e2e-local` | local or remote k3d | reconciler, login wall, cookie stripping, templates, MCP tools | minutes | not built yet |
| L2 | `make e2e-hetzner` | throwaway Hetzner tenant | see below | about 6 minutes | working |
| L3 | `make acceptance` | Hetzner | all acceptance tests in [roadmap.md](roadmap.md) | about an hour, nightly | not built yet |

When to run what:

- Every change: L0 (and L1 once it exists).
- Changes to provisioning, networking, storage, TLS or recovery: L2 as well.
- Nightly: L3.

Every failed e2e run writes cluster state, events and logs to `test/artifacts/<run-id>/` so the agent can debug from them.

### What the L2 smoke test proves

`make e2e-hetzner` runs the reaper, then:

1. Creates a per-run SSH key, private network, firewall, 3 Debian servers (k3s HA with embedded etcd), a load balancer and a wildcard DNS record `*.<run-id>.test.slicedbread.dev`.
2. Installs cert-manager and the Hetzner CSI driver, and issues a wildcard certificate from Let's Encrypt staging through Cloudflare DNS-01. Traefik serves it as the default certificate.
3. Deploys `whoami` (restricted pod security) and a pod writing to a 10GB Hetzner volume.
4. Checks that DNS resolves to the load balancer, that HTTPS works with a certificate that chains to the Let's Encrypt staging roots and matches the host, and that internal ports (6443, 10250, NodePorts) are not reachable on the node public IPs.
5. Destroys everything, including volumes created by Kubernetes, and fails if any labelled resource remains.

## Scheduled reaper

A launchd agent on the Mac mini runs the reaper every 15 minutes.

```sh
make reaper-schedule     # build sb, install and load the agent (rerun after pulling reaper changes)
make reaper-unlock       # once after every reboot: unlock the dev keychain for the agent
make reaper-status       # agent state, last exit code and the end of the log
make reaper-unschedule   # unload and remove it (the log stays)
```

How it works:

- The agent is `~/Library/LaunchAgents/dev.slicedbread.reaper.plist`, loaded into the per-user background launchd domain (`user/<uid>`, `LimitLoadToSessionType` `Background`). It runs at load and then every 900 seconds. launchd never starts a second copy while one is running, and the lock above also excludes manual runs.
- It runs a prebuilt `sb` (no Go toolchain needed) from `~/Library/Application Support/SlicedBread/reaper/`, which holds `bin/sb`, `hack/secrets-env`, `hack/reaper-agent` and a copy of `dev.env`. It cannot run from the checkout: macOS privacy protection blocks launchd jobs from reading `~/Documents`. `make reaper-schedule` copies all of it there, so the checkout it ran from can be removed later. Rerun it after pulling changes to the reaper.
- Each run appends to `~/Library/Logs/slicedbread-reaper.log`: a start line, the inventory summary (labelled resources, runs, expired runs), one line per deleted run and resource pass, and a `finished, exit N` line. Every line has a timestamp. When the log reaches 1 MB it is moved to `slicedbread-reaper.log.1` (one old copy is kept). A failed run exits non-zero, which `make reaper-status` shows as the last exit code.
- Keychain: unlock state is per login session, and launchd jobs do not run in the session of your SSH or terminal shell. Unlocking the keychain in your shell does not unlock it for the agent. `make reaper-unlock` asks for the keychain password and hands it to a one-shot job in the agent's session through a FIFO only you can open, so it never appears on a command line or in a file. It stays unlocked for the agent until the next reboot.
- When the keychain is locked for the agent, the run logs `keychain slicedbread-dev.keychain-db is locked in the reaper's launchd session; run: make reaper-unlock` and exits 3. The background session has no window server, so it fails at once and never waits on a password prompt.
- Each run is limited to 30 minutes.

## Network exposure of development tenants

- Nodes have a Hetzner firewall with one inbound rule: SSH from the operator's current public IP. Nothing else is reachable on node public IPs.
- HTTP and HTTPS reach Traefik through the load balancer over the private network (NodePorts 30080 and 30443). Hetzner firewalls do not filter private network traffic.
- SSH uses a per-run key stored in `.sb/runs/<run-id>/` (gitignored) and host keys generated by the provisioner and pinned, so there is no trust on first use.
- Known gap: Hetzner user data (k3s join token, SSH host key) is readable from inside the servers through the metadata service, including from pods. Acceptable for short-lived development tenants. Production must block pod access to the metadata service or deliver these differently.

## Guardrails against cost and leftovers

- Every cloud resource gets the labels `sb/run-id`, `sb/owner` and `sb/expires-at` (unix seconds). DNS records carry the same values in their comment. Volumes created by the CSI driver get them through `volumeExtraLabels`.
- Default lifetime is 2 hours (`SB_TTL`, maximum 24h).
- `make reaper` deletes every run that has an expired resource. It runs before every `make e2e-hetzner` and every 15 minutes on the Mac mini (see "Scheduled reaper" below), so a run killed mid-way stops billing within about 15 minutes of its expiry.
- The reaper never deletes a run that has not expired. A run is deleted when one of its resources carries an `sb/expires-at` in the past. Resources without a usable expiry (half-labelled leftovers of a crashed run, stray `_acme-challenge` records, whose expiry is their age plus 2 hours) only count when no resource of the same run carries a future expiry. A run with any resource labelled `sb/expires-at=never` (a real install) is never deleted; the reaper logs it if other resources of that run look expired.
- Reaper runs never overlap: each holds an exclusive lock on `~/Library/Caches/slicedbread-reaper.lock` (any checkout, manual or scheduled). `make reaper` waits up to 20 minutes for a running one; the scheduled run skips instead.
- Every run destroys what it created on exit, success or failure. `SB_KEEP_ON_FAILURE=1` keeps a failed tenant for debugging until the reaper removes it.
- Approximate cost: 3 x cx23 plus lb11 is about EUR 0.045 per hour. A smoke run costs about one cent.

## Repository layout

Current layout is described in [AGENTS.md](../AGENTS.md). Two binaries ship to users: `sb` (`cmd/sb`, the CLI) and `sb-control` (`cmd/sb-control`, which runs in each install). Everything only developers need is under `hack/`: the `sb-dev` binary (`hack/sb-dev`: test installs, prototypes, pushing sb-control to a test install, the reaper, the smoke test), the smoke test logic (`hack/smoke`), and scripts (`secrets-env`, `scan-secrets`, the scheduled reaper). It stays in the repository so anyone can reproduce our tests and debug an install the way we do. The Makefile targets run `sb-dev` for you; `make build` also builds `bin/sb-dev`.

## Order of work

1. Done: repo skeleton, Makefile, `AGENTS.md`, CI for L0.
2. Done: provisioner, reaper, L2 smoke test.
3. Done: prototype platform (`make prototype-up`, `make control-push`): sb-control, login wall, approvals, builds, uploads, templates, CLI, skill file, browser login.
4. Done: agent tests. Subagents given only `skill/SKILL.md` and the CLI deployed a Lovable export, added Umami analytics and set up Vaultwarden. Their reports drove two rounds of fixes.
5. Next: backups and restore tests (M6), node replacement, automatic rollback, an automated template verification workflow, and turning the agent tests into a repeatable suite.

## Agent tests

The prototype's main QA loop is an LLM agent operating a live tenant with nothing but the skill file and the CLI:

1. `make prototype-up`, then copy `bin/sb`, `skill/SKILL.md` and `.sb/runs/<id>/cli-agent.json` into an empty directory.
2. Give a subagent a user request ("here's my Lovable export, run it, team only"), the paths, and rules: only the CLI, the skill file and the user's project; no source code, kubectl or SSH; never print secrets.
3. Ask for a report: commands and results, final state, and every point of confusion. Fix the product, not the agent.

## Testing `sb init` (self-hosted installs)

`sb init` is the user-facing installer. To test it with development credentials, use a name under `test.slicedbread.dev` as the user's domain and label the resources to expire, so the reaper removes them if a test is abandoned:

```sh
make build
SB_STATE_DIR=$PWD/.sb/installs SB_CONFIG=$PWD/.sb/cli/one.json \
  hack/secrets-env bin/sb init --domain one.test.slicedbread.dev --size 1 --yes --expire-after 4h
```

`hack/secrets-env` provides `HCLOUD_TOKEN` and `CF_API_TOKEN` (which `sb init` accepts as the Cloudflare token). For backups to the dev bucket, map `SB_S3_ACCESS_KEY`/`SB_S3_SECRET_KEY` to `SB_BACKUP_ACCESS_KEY_ID`/`SB_BACKUP_SECRET_ACCESS_KEY` and use `--backup-endpoint https://s3.duti.dev --backup-bucket sb-dev --backup-region garage --backup-prefix test/<name>` (endpoint and bucket are in `dev.env`).

Install state (SSH key, credentials) lives in `SB_STATE_DIR` (default `~/.config/slicedbread/installs`). `sb upgrade <id>` pushes a rebuilt sb-control, `sb ssh <id>` prints an SSH command, `sb destroy <id> --yes` removes everything. Real installs are labelled `sb/expires-at=never` and are never reaped.
