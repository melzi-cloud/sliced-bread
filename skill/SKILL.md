---
name: slicedbread
description: Deploy and operate apps (off-the-shelf software, container images, git repos, local directories such as a Lovable export) on a SlicedBread tenant using the `sb` CLI. Use when asked to host, run, deploy, add analytics, or manage self-hosted apps.
---

# SlicedBread

SlicedBread runs apps on the user's own private cluster. Every app gets `https://<app>.<tenant-domain>` with a trusted certificate. Apps are **private by default**: a login wall in front of the app lets only signed-in users in. Today that is a single admin account (no team members yet), so say "only you" rather than "your team".

You operate it with the `sb` CLI. You never need kubectl, SSH or cloud consoles.

## Before you start

```sh
sb whoami      # tenant domain, the team the login wall admits, approvals URL
sb apps        # what is already running
sb catalog     # off-the-shelf templates (skip if you are deploying the user's own code)
```

The tenant is shared: the user or other agents may deploy apps while you work. Only report on apps and approvals you created.

Output is human-readable. Add `--json` to any command for JSON. `sb <command> --help` shows that command's usage.

If there is no SlicedBread install yet and the user asks for one, see "Installing" below.

If `sb whoami` fails with "not logged in", ask the user for the API URL (`https://api.<tenant-domain>`) and run `sb login --api <url>`. It prints a link and a code; give both to the user, who signs in and clicks Allow. The CLI then stores its own agent token. Never ask the user to paste a token into the chat.

## Choosing how to deploy

| The user wants | Command |
|---|---|
| Known software (password manager, analytics) | `sb deploy <app> --template <name>` (check `sb catalog` first) |
| Their own code in a local folder (e.g. a Lovable export) | `sb deploy <app> --dir <path>` |
| A public git repo | `sb deploy <app> --repo https://github.com/org/repo` |
| A prebuilt container image | `sb deploy <app> --image <image> --port <port>` |

After every deploy or redeploy, run `sb wait <app>`. It returns only when the **latest** version is built, rolled out and ready (states it passes through: `building`, `starting`, `updating`, `configuring`), and ends with `ready: <url>` (plus the build id now serving, for source apps). If the new build fails it exits 1, even though the previous version keeps serving (`update-failed`). Source builds take up to 5 minutes (seconds when nothing changed). Databases take 1 to 2 minutes to come up.

Then verify the app really serves what you expect with `sb check <app> [/path] [--contains text]`. It fetches the page from inside the cluster, past the login wall, so it works for private apps. Use it to confirm a page renders, that client-side routes fall back to `index.html` (`sb check myapp /some/route`), and that an edit made it into the deployed version (`--contains`).

App names: 2 to 30 lowercase letters, digits or dashes, starting with a letter. `api`, `auth` and a few others are reserved.

### Source builds (`--dir`, `--repo`)

- Built in the cluster with Railpack (detects Node, Vite/React single-page apps, Python, Go and more). If the folder has a `Dockerfile`, it is used instead.
- The app must listen on `$PORT` (set to 8080). Railpack handles this for static sites and most frameworks. For a Dockerfile app that listens elsewhere, pass `--port`.
- Needs Postgres? Add `--database`: a Postgres database is created next to the app and `DATABASE_URL` is set (a `postgresql://` URL).
- Has a health endpoint? Add `--health-path /healthz` so a new version only takes traffic once it answers there (default: the port accepts connections).
- `--dir` skips `node_modules`, `.git`, `dist`, `build`, `.next`, `.venv`, `target` and similar folders. Everything else is uploaded, including `.env` files, because Vite and similar tools read them at build time. Do not keep server-side secrets in uploaded files; use `sb env set --secrets-stdin`. Preview the file list with `--dry-run`. A `.dockerignore` in the project is respected by the build. You do not need to build locally.
- `sb status <app>` shows the content sha256 of the uploaded source and the build id serving. `sb deploy <app> --dir <path> --dry-run` prints the content sha256 of the folder as it is now (file contents only, timestamps ignored). Equal hashes mean the deployed source matches the folder. The app name can differ from the folder name.
- To ship code changes: `sb redeploy <app> --dir <path>` (or `sb redeploy <app>` for a repo, which pulls the latest commit). The previous version keeps serving until the new build is ready.
- If a build fails: `sb logs <app> --build` shows the clone, plan and build output.

## Access modes

| Mode | Meaning |
|---|---|
| `wall` (default) | Visitors must sign in to SlicedBread first. Use for internal tools and dashboards. |
| `app-auth` | No SlicedBread wall; the app's own login protects it. Templates set this when needed (Vaultwarden, because Bitwarden apps cannot pass a browser login). |
| `public` | Anyone can reach it. |

Wall apps can expose specific paths publicly with `--public-path /path` (for webhooks or tracking scripts).

Change access later with `sb access <app> wall|app-auth|public [--public-path /x]`.

**Making an app reachable by more people needs a human.** These return **exit code 3** with a pending approval:
- deploying with `--access public` or `--access app-auth` yourself (access set by a template does not need approval)
- `sb access` to a wider mode, or adding public paths
- template settings marked "needs human approval" in `sb catalog` (e.g. Vaultwarden signups=true)
- `sb delete`

Narrowing access (back to `wall`, signups=false) never needs approval.

When you see exit code 3:

1. Tell the user what you requested and give them the `approve at:` URL from the output.
2. Do not try to work around it (for example by redeploying under another name or with another mode). You cannot approve; `sb approve` is for humans with an admin token.
3. Wait with `sb approvals --wait <id> [--timeout 30m]`. It exits 0 when approved, 1 when denied or obsolete, and 3 if still pending at the timeout. Or stop and report. `sb approvals --app <app>` lists one app's approvals. Once approved, the change applies automatically. `sb status <app>` lists pending approvals for the app.
4. If the app changes or is deleted before approval, the approval becomes obsolete and must be requested again.

### How the wall answers

A request without a SlicedBread session gets a `302` redirect to the login page if it looks like a browser (`Accept: text/html`), otherwise `401`. Both mean the wall works. To see the app's content, use `sb check`.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | OK |
| 1 | Error: includes a failed build, a denied or obsolete approval, and `sb check --contains` not finding the text |
| 2 | Usage error (`sb <command> --help` shows the full usage) |
| 3 | Waiting for human approval, including `sb approvals --wait` timing out |

## Configuration

```sh
sb env <app>                                   # list values (secrets show as <secret>)
sb env set <app> KEY=value OTHER=value         # plain values; pods restart
printf 'API_KEY=%s\n' "$VALUE" | sb env set <app> --secrets-stdin   # secret values via stdin only
sb env unset <app> KEY
```

Values can reference:
- `${app.url}`: this app's public URL
- `${apps.<other>.url}`: another app's public URL
- `${apps.<other>.internal}`: another app's in-cluster URL (no login wall), e.g. for an API backend. Referencing it is what allows the traffic: apps can only reach apps they reference this way.
- `${secret.NAME}`: a secret value, must be the whole value

Never put secrets on the command line or in plain `--env` values. Use `--secrets-stdin`.

Note: environment variables are runtime only. Frameworks that bake env into the bundle at build time (Vite `VITE_*`) need the value in the source (for example a committed `.env`) before `sb redeploy`.

## Templates

Run `sb catalog` for the current list. Each template documents its settings, actions and notes. `sb status <app>` also shows them for a deployed app.

### Template actions

Some templates expose typed actions so you do not have to click through UIs:

```sh
sb action <app> <action> --input key=value
```

Actions marked `adminOnly` are for the human; if you get "admin token only", tell the user to run it.

### Example: add analytics to an app

```sh
sb deploy analytics --template umami
sb wait analytics          # includes template setup; actions work once this returns
sb action analytics create_website --input name="My App" --input domain=myapp.<tenant-domain>
# output includes "snippet": a <script> tag
```

Then add the snippet inside `<head>` of the app's HTML (for Vite/React/Lovable projects: `index.html` at the project root), ship it with `sb redeploy myapp --dir <path>`, run `sb wait myapp`, and verify with `sb check myapp --contains data-website-id`. Confirm events are counted with `sb action analytics test_event` (adds one page view at `/sb-test-event`, tell the user), and read counts with `sb action analytics stats`.

Tell the user two things:
- If the tracked app is behind the wall, only signed-in team members' visits are counted. Real visitor numbers need the app to be `public` (`sb access myapp public`, needs approval). Do not request that unless the user wants it.
- The Umami dashboard has its own login, available to the human with `sb action analytics dashboard_login` (admin token).

### Example: password manager

```sh
sb deploy vault --template vaultwarden
sb wait vault
sb settings vault signups=true      # exit code 3: give the user the approve URL
sb approvals --wait <id>            # once approved, the user registers at the app URL
sb settings vault signups=false     # after the account exists (no approval needed to close)
```

Signups close automatically 15 minutes after the change is applied (that is, after the human approves), so tell the user to register right away. To narrow who can register, also set `signup_domain=<company domain>` (for example `sb settings vault signups=true signup_domain=acme.com`).

Tell the user: in the Bitwarden app or browser extension, choose "Self-hosted" and enter the app URL.

## Data and backups

Templates that keep data (Vaultwarden, Umami's Postgres) use persistent volumes that survive restarts and redeploys. Deleting an app deletes its data.

When backups are configured (`sb backups` says so), every app with a volume or database is backed up daily, and **each backup is followed by a restore test** that restores it into a scratch namespace and checks it. New apps get their first backup soon after they are ready.

```sh
sb backups                 # target, schedule, per app: last backup and last restore test
sb backups <app>           # plus the list of snapshots
sb backup <app> --wait     # back up now (then the restore test runs); exit 1 if either fails
sb restore-test <app> [--snapshot ID] --wait   # test any snapshot again without touching the app
sb restore <app> [--snapshot ID] --wait   # replace the app's data with a snapshot (default: latest)
```

What a restore test checks, so you can report it accurately: restic restores the snapshot into a scratch namespace and verifies every file's content against the backup; the number of restored files must match the backup; template-required files must exist and be non-empty (Vaultwarden: `db.sqlite3`; its SQLite integrity is not checked); a database dump must load into a fresh Postgres with the same number of tables (rows are counted and reported). The first backup of a new app runs once it is running and its template setup has finished (trigger `initial`).

- A restore stops the app, replaces its volumes and database, and starts it again. With an agent token it needs human approval (exit code 3); tell the user what will be lost (everything since that snapshot).
- Apps that keep SQLite files (Vaultwarden) are stopped for a few seconds during each backup. `sb status` then shows `paused`; it resumes by itself.
- If backups are not configured, tell the user: an admin runs `sb backups configure` (or passed backup flags to `sb init`). Until then nothing is backed up.
- `sb export <dir>` (admin token) writes every app as plain Kubernetes manifests.

## Installing

Only when the user asks for a new install. It runs from the user's machine against their own Hetzner account and Cloudflare domain:

```sh
sb init --domain apps.example.com --email you@example.com --size 1 \
  [--backup-endpoint https://<s3 endpoint> --backup-bucket <bucket>]
```

Secrets come from environment variables, never flags: `HCLOUD_TOKEN`, `CLOUDFLARE_API_TOKEN` (or `CF_API_TOKEN`), and for backups `SB_BACKUP_ACCESS_KEY_ID` and `SB_BACKUP_SECRET_ACCESS_KEY`. Never print them. `--size 1` is one server (about EUR 7/month, no high availability), `--size 3` is three servers and a load balancer (about EUR 29/month). It takes 5 to 10 minutes, then the CLI is logged in with an **admin** token, which approves its own requests. Before doing anything else, switch yourself to the agent token that `sb init` printed (`export SB_CONFIG=<install dir>/cli-agent.json`) so exposure changes, deletes and restores wait for the user, as the rest of this file assumes. Admin-only steps (`sb backups configure`, `sb export`, approvals) are for the user. Tell the user where the dashboard login and the backup encryption password were saved (printed at the end), and that `sb destroy <install-id>` removes everything.

## Troubleshooting

| Symptom | Do this |
|---|---|
| `sb wait` shows `build-failed` | `sb logs <app> --build`, fix the source or add a Dockerfile, `sb redeploy <app> --dir <path>` |
| `starting` for several minutes | `sb status <app>` (message explains: image pull, crash, waiting for database), then `sb logs <app>` |
| `CrashLoopBackOff` / restarts | `sb logs <app>`; usually missing env or wrong port (`--port`) |
| `error: app ... already exists` | Use `sb redeploy`, `sb env`, or `sb settings`; deleting needs approval |
| Exit code 3 | Waiting for a human; see Access modes above |
| `running` but the page looks wrong | `sb check <app> /` and `sb logs <app>` |

## Rules

- Prefer templates over hand-rolled images when one exists.
- Default to `wall`. Only request wider access when the user asked for it or the template requires it.
- Report the app URL to the user when you finish, and say whether it is behind the login wall.
- Do not print or echo secret values.
- Use `--json` on any command if you need machine-readable output.
