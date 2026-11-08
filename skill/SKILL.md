---
name: slicedbread
description: Deploy and operate apps (off-the-shelf software, container images, git repos, local directories such as a Lovable export) on a SlicedBread tenant using the `sb` CLI. Use when asked to host, run, deploy, add analytics, or manage self-hosted apps.
---

# SlicedBread

SlicedBread runs apps on the user's own private cluster. Every app gets `https://<app>.<tenant-domain>` with a trusted certificate. Apps are **private by default**: a login wall in front of the app lets only signed-in members of the install in (`sb members` lists them). Admins invite people with `sb members invite <email>`; the invite link is sent by the user, not by SlicedBread.

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
- `--dir` skips `node_modules`, `.git`, `dist`, `build`, `.next`, `.venv`, `target` and similar folders. Everything else is uploaded, including `.env` files, because Vite and similar tools read them at build time. Do not keep server-side secrets in uploaded files; use `sb env generate` or `sb env request` (see Configuration). Preview the file list with `--dry-run`. A `.dockerignore` in the project is respected by the build. You do not need to build locally.
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
- deploying with `--access public` or `--access app-auth` yourself (access set by a catalog template does not need approval; a template from `--template-dir` does)
- `sb access` to a wider mode, or adding public paths
- template settings or actions marked "needs human approval" in `sb catalog`
- `sb delete`

Narrowing access (back to `wall`) never needs approval.

When you see exit code 3:

1. Tell the user what you requested and give them the `approve at:` URL from the output.
2. Do not try to work around it (for example by redeploying under another name or with another mode). You cannot approve: humans approve in the dashboard, and no CLI token can.
3. Wait with `sb approvals --wait <id> [--timeout 30m]`. It exits 0 when approved, 1 when denied or obsolete, and 3 if still pending at the timeout. Or stop and report. `sb approvals --app <app>` lists one app's approvals. Once approved, the change applies automatically. `sb status <app>` lists pending approvals for the app.
4. If the app changes or is deleted before approval, the approval becomes obsolete and must be requested again. A redeploy (new code), `sb env` or `sb settings` counts as a change, so finish code and config changes first and request wider access last.
5. If nobody is watching (for example you run unattended), do not block for long: request, report the approve link, and stop. `sb approvals --wait` is for when the user is at hand.

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
sb env set <app> KEY=value OTHER=value         # plain, non-secret values only; pods restart
sb env generate <app> SESSION_SECRET           # the server creates a random secret; you never see it
sb env request <app> STRIPE_KEY --description "Stripe secret key for payments"   # the user enters it in the dashboard
sb env unset <app> KEY
```

**You never see, type or generate secret values.** Do not invent a random string yourself (text from a language model is not random enough) and do not ask the user to paste a secret into the chat. If the app needs a random secret nobody has to read, use `sb env generate`. If it needs a value only the user has (an API key, a password), use `sb env request` and give the user the link it prints. Until the user enters it, `sb env` shows it as `<not set: ...>` and `sb status <app>` lists it as a setup step that is not done. If the user pastes a secret into the chat anyway, do not use it: request it and tell them to rotate the pasted one. `sb deploy` takes the same as flags: `--generate KEY` and `--request KEY:description`.

Values can reference:
- `${app.url}`: this app's public URL
- `${apps.<other>.url}`: another app's public URL
- `${apps.<other>.internal}`: another app's in-cluster URL (no login wall), e.g. for an API backend. Referencing it is what allows the traffic: apps can only reach apps they reference this way.
- `${secret.NAME}`: a secret value, must be the whole value

Note: environment variables (and secrets) only reach code that runs on the server. A static site (a Vite/React/Lovable export with no backend) never reads them, and a secret such as a Stripe secret key must never go into a browser bundle. Environment variables are runtime only. Frameworks that bake env into the bundle at build time (Vite `VITE_*`) need the value in the source (for example a committed `.env`) before `sb redeploy`.

## Templates

Run `sb catalog` for the current list. `sb catalog --refresh` fetches the latest signed catalog (new templates and template versions); apps already deployed keep the version they were created from. Each template documents its components, settings, actions, setup steps and notes. `sb status <app>` shows them for a deployed app, plus which setup steps the user still has to do.

### Template actions

Templates expose typed actions so you do not have to click through UIs. Each runs as a short job next to the app (a few seconds):

```sh
sb action <app> <action> --input key=value
```

- Actions marked "runs by itself when the app is installed" are run by SlicedBread; `sb wait` returns once they succeeded. If one keeps failing, `sb status` says why, and `sb logs <app> --action <name>` shows its output.
- Actions marked "shows credentials" return a value only a human may see. You get the other fields and a dashboard link instead; give the user that link (`https://auth.<tenant-domain>/apps/<app>`).
- Secret values never appear in action output: if one would, it shows as `[secret]`.

### Setup steps

Some templates need the user to do something once, for example create the first account. `sb status <app>` shows "setup for the user (not done yet)" with a link: give the user that exact link (it signs them in first). The step disappears by itself when it is done; nothing needs confirming.

### Example: add analytics to an app

```sh
sb deploy analytics --template umami
sb wait analytics          # includes template setup; actions work once this returns
sb action analytics create_website --input name="My App" --input domain=myapp.<tenant-domain>
# output includes "snippet": a <script> tag
```

Then add the snippet inside `<head>` of the app's HTML (for Vite/React/Lovable projects: `index.html` at the project root), ship it with `sb redeploy myapp --dir <path>`, run `sb wait myapp`, and verify with `sb check myapp --contains data-website-id`. Confirm Umami counts events with `sb action analytics test_event` (adds one page view at `/sb-test-event`, tell the user; it is sent from the server, so it proves Umami works, not that a visitor's browser loads the script: `sb check myapp --contains data-website-id` and a public `/script.js` cover that part), and read counts with `sb action analytics stats`.

Tell the user two things:
- If the tracked app is behind the wall, only signed-in team members' visits are counted. Real visitor numbers need the app to be `public` (`sb access myapp public`, needs approval). Do not request that unless the user wants it.
- The Umami dashboard has its own login; the human gets it from the `dashboard_login` action on the app's page in the dashboard.

### Example: password manager

```sh
sb deploy vault --template vaultwarden
sb wait vault
sb status vault     # "setup for the user": give the user the link to create their account
```

Only members of this install can create Vaultwarden accounts: registration needs the SlicedBread login, the rest of Vaultwarden does not, so the Bitwarden apps keep working. There is no signup switch to open or close. To limit accounts to company addresses: `sb settings vault signup_domain=acme.com`. Tell the user: in the Bitwarden app or browser extension, choose "Self-hosted" and enter the app URL.

### Writing a template

When the user wants off-the-shelf software that has no template, you can write one: a directory with `template.yaml` (format: `bakery.slicedbread.dev/v1`, described in `template-format.md` next to this file, or `docs/template-format.md` in the SlicedBread repository; the templates in https://github.com/melzi-cloud/bakery are complete working examples), action scripts under `actions/` and config files under `files/`. Pin every image by digest (`name:tag@sha256:...`). Deploy it with `sb deploy <app> --template-dir <path>`; errors in the template are reported with what to fix. A template from a directory is not trusted: an access default other than `wall`, and its public paths, need human approval (a `wall` template deploys straight away). Prefer catalog templates when one exists.

## Data and backups

Templates that keep data (Vaultwarden, Umami's Postgres) use persistent volumes that survive restarts and redeploys. Deleting an app deletes its data.

When backups are configured (`sb backups` says so), every app with a volume or database is backed up on the schedule an admin sets in the dashboard (daily at 03:00 UTC unless changed; `sb backups` shows it), and **each backup is followed by a restore test** that restores it into a scratch namespace and checks it. New apps get their first backup soon after they are ready.

```sh
sb backups                 # target, schedule, per app: last backup and last restore test
sb backups <app>           # plus the list of snapshots
sb backup <app> --wait     # back up now (then the restore test runs); exit 1 if either fails
sb restore-test <app> [--snapshot ID] --wait   # test any snapshot again without touching the app
sb restore <app> [--snapshot ID] --wait   # replace the app's data with a snapshot (default: latest)
sb backups --repository    # every app in the backup bucket, including deleted ones
sb restore <app> --recreate [--snapshot ID] --wait   # bring back a deleted app: spec, secrets, source and data
```

Each snapshot carries the app's spec, its secret values and, for apps deployed with `--dir`, the uploaded source, so a deleted app can come back from the bucket alone. With an agent token `--recreate` needs human approval (exit code 3). The recreated app starts only after its data is restored. Apps without a volume or database are backed up too (settings and source only), so they can be recreated the same way. After a whole install was rebuilt under a new domain, check apps whose source contains URLs of other apps (tracking snippets, API base URLs) and redeploy them with the new URLs.

What a restore test checks, so you can report it accurately: restic restores the snapshot into a scratch namespace and verifies every file's content against the backup; the number of restored files must match the backup; template-required files must exist and be non-empty (Vaultwarden: `db.sqlite3`); SQLite files the template declares must pass `PRAGMA integrity_check` and contain the declared tables, whose row counts are reported (Vaultwarden: `users` is accounts, `ciphers` is vault items; 0 users is normal before the first account and does not fail the test); a database dump must load into a fresh Postgres with the same number of tables (rows are counted and reported). The first backup of a new app runs once it is running and its template setup has finished (trigger `initial`).

- A restore stops the app, replaces its volumes and database, and starts it again. With an agent token it needs human approval (exit code 3); tell the user what will be lost (everything since that snapshot).
- Apps that keep SQLite files (Vaultwarden, templates with `consistency: stop`) are stopped for a few seconds during each backup, including the first one right after `sb wait` reports ready. `sb status` then shows `paused`; it resumes by itself.
- If backups are not configured, tell the user to set up a bucket on the dashboard's Settings page. Until then nothing is backed up.
- Restoring every app from the bucket (for example into a new install) and exporting manifests are dashboard actions for the human.

## Humans and agents

You work through the CLI with an agent token. A human does admin work in the dashboard at `https://auth.<tenant-domain>`: approving your requests, members, backup settings and the encryption password, restoring everything from the bucket, alert webhooks, export, and actions that show credentials. Commands for those (`sb approve`, `sb backups configure`, `sb export`, ...) print the dashboard link instead; pass it to the user.

## Members

```sh
sb members                       # who gets past the login wall, and whether members can approve
```

Inviting, removing and changing roles is done by an admin on the dashboard's Members page; give the user that link (`sb members` prints it).

## Alerts

```sh
sb alerts        # open alerts: failed backups or restore tests, apps down for 15 minutes, certificates, servers
```

The dashboard at `https://auth.<tenant-domain>/` shows the same list. Each alert has a `next:` hint with the command to investigate. Check `sb alerts` at the start of operational work and after finishing a task; report open alerts to the user. Admins can send alerts to a webhook (Slack, Discord, ntfy or JSON) on the dashboard's Settings page.

## Maintenance

The install patches itself: every night, after a fresh backup and restore test of every app with data, security updates install, services using replaced libraries restart, and servers that need it reboot one at a time; a failed backup postpones the updates and opens an alert; on 3 servers a dead server is replaced automatically. `sb status <app>` shows `update:` when a newer upstream version of a template's image exists; it is not applied automatically, so mention it to the user rather than redeploying. Alerts about server replacement or failed maintenance appear in `sb alerts`.

## Installing

Only when the user asks for a new install. It runs from the user's machine against their own Hetzner account and Cloudflare domain. If `sb` is missing, install it (`sb version` shows the installed version):

```sh
curl -fsSL https://raw.githubusercontent.com/melzi-cloud/sliced-bread/main/install.sh | sh
```

Then ask the user to run this in their own terminal (it asks for the Hetzner and Cloudflare tokens, and S3 keys with `--backup-bucket`, without echoing them, so the tokens never pass through you):

```sh
sb init --domain apps.example.com --size 1 \
  [--backup-endpoint https://<s3 endpoint> --backup-bucket <bucket>]
```

For a scripted install the tokens come from environment variables, never flags: `HCLOUD_TOKEN`, `CLOUDFLARE_API_TOKEN` (or `CF_API_TOKEN`), and for backups `SB_BACKUP_ACCESS_KEY_ID` and `SB_BACKUP_SECRET_ACCESS_KEY`. Never print them. `--size 1` is one server (about EUR 7/month, no high availability), `--size 3` is three servers and a load balancer (about EUR 29/month). It takes 5 to 10 minutes. At the end it prints two things only the human should handle: a one-time **setup link** (the user opens it and creates the owner account) and the **backup encryption password** (the user saves it in their password manager; do not repeat it, store it or put it anywhere else). Tell the user to do both. The CLI is then logged in with an agent token, which is all you need; everything admin happens in the dashboard. `sb setup-link <install-id>` prints a fresh setup link if the first one expired; `sb destroy <install-id>` removes everything.

## Troubleshooting

| Symptom | Do this |
|---|---|
| `sb wait` shows `build-failed` | `sb logs <app> --build`, fix the source or add a Dockerfile, `sb redeploy <app> --dir <path>` |
| `starting` for several minutes | `sb status <app>` (message explains: image pull, crash, waiting for database), then `sb logs <app>` |
| `CrashLoopBackOff` / restarts | `sb logs <app>`; usually missing env or wrong port (`--port`) |
| `error: app ... already exists` | Use `sb redeploy`, `sb env`, or `sb settings`; deleting needs approval |
| Exit code 3 | Waiting for a human; see Access modes above |
| `running` but the page looks wrong | `sb check <app> /` and `sb logs <app>` |

## Secrets the user pastes into the chat

If the user pastes a key or password (S3 keys, API keys) into the conversation, do not pass it to commands you were not asked to run, do not store it in files, and do not repeat it. Where it belongs in the dashboard (backup bucket keys, members), give the link instead, and suggest rotating anything that was pasted. App secrets an app needs (for example a Stripe key) go in through `sb env request <app> KEY`: the user enters the value in the dashboard, and should rotate any key already pasted into the chat.

## Rules

- Prefer templates over hand-rolled images when one exists.
- Default to `wall`. Only request wider access when the user asked for it or the template requires it.
- Report the app URL to the user when you finish, and say whether it is behind the login wall.
- Do not print or echo secret values.
- Use `--json` on any command if you need machine-readable output.
