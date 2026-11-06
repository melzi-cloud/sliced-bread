---
name: slicedbread
description: Deploy and operate apps (off-the-shelf software, container images, git repos, local directories such as a Lovable export) on a SlicedBread tenant using the `sb` CLI. Use when asked to host, run, deploy, add analytics, or manage self-hosted apps.
---

# SlicedBread

SlicedBread runs apps on the user's own private cluster. Every app gets `https://<app>.<tenant-domain>` with a trusted certificate. Apps are **private by default**: a login wall in front of the app lets only the user's team in.

You operate it with the `sb` CLI. You never need kubectl, SSH or cloud consoles.

## Before you start

```sh
sb whoami      # tenant domain, the team the login wall admits, approvals URL
sb apps        # what is already running
sb catalog     # off-the-shelf templates (skip if you are deploying the user's own code)
```

The tenant is shared: the user or other agents may deploy apps while you work. Only report on apps and approvals you created.

Output is human-readable. Add `--json` to any command for JSON. `sb <command> --help` shows that command's usage.

If `sb whoami` fails with "not logged in", ask the user for the API URL (`https://api.<tenant-domain>`) and run `sb login --api <url>`. It prints a link and a code; give both to the user, who signs in and clicks Allow. The CLI then stores its own agent token. Never ask the user to paste a token into the chat.

## Choosing how to deploy

| The user wants | Command |
|---|---|
| Known software (password manager, analytics) | `sb deploy <app> --template <name>` (check `sb catalog` first) |
| Their own code in a local folder (e.g. a Lovable export) | `sb deploy <app> --dir <path>` |
| A public git repo | `sb deploy <app> --repo https://github.com/org/repo` |
| A prebuilt container image | `sb deploy <app> --image <image> --port <port>` |

After every deploy, run `sb wait <app>`. It prints progress and ends with `ready: <url>` or an error with a hint. Source builds take up to 5 minutes (seconds when nothing changed). Databases take 1 to 2 minutes to come up.

Then verify the app really serves what you expect with `sb check <app> [/path] [--contains text]`. It fetches the page from inside the cluster, past the login wall, so it works for private apps. Use it to confirm a page renders, that client-side routes fall back to `index.html` (`sb check myapp /some/route`), and that an edit made it into the deployed version (`--contains`).

App names: 2 to 30 lowercase letters, digits or dashes, starting with a letter. `api`, `auth` and a few others are reserved.

### Source builds (`--dir`, `--repo`)

- Built in the cluster with Railpack (detects Node, Vite/React single-page apps, Python, Go and more). If the folder has a `Dockerfile`, it is used instead.
- The app must listen on `$PORT` (set to 8080). Railpack handles this for static sites and most frameworks. For a Dockerfile app that listens elsewhere, pass `--port`.
- Needs Postgres? Add `--database`: a Postgres database is created next to the app and `DATABASE_URL` is set (a `postgresql://` URL).
- Has a health endpoint? Add `--health-path /healthz` so a new version only takes traffic once it answers there (default: the port accepts connections).
- `--dir` skips `node_modules`, `.git`, `dist`, `build`, `.next`, `.venv`, `target` and similar folders. Everything else is uploaded, including `.env` files, because Vite and similar tools read them at build time. Do not keep server-side secrets in uploaded files; use `sb env set --secrets-stdin`. Preview the file list with `--dry-run`. A `.dockerignore` in the project is respected by the build. You do not need to build locally.
- `sb status <app>` shows the sha256 of the uploaded source, so you can confirm which upload is deployed.
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
3. Wait with `sb approvals --wait <id>` (blocks until a human decides, default timeout 30 minutes), or stop and report. Once approved, the change applies automatically. `sb status <app>` lists pending approvals for the app.
4. If the app changes or is deleted before approval, the approval becomes obsolete and must be requested again.

### How the wall answers

A request without a SlicedBread session gets a `302` redirect to the login page if it looks like a browser (`Accept: text/html`), otherwise `401`. Both mean the wall works. To see the app's content, use `sb check`.

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
- `${apps.<other>.internal}`: another app's in-cluster URL (no login wall), e.g. for an API backend
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
sb wait analytics
sb action analytics create_website --input name="My App" --input domain=myapp.<tenant-domain>
# output includes "snippet": a <script> tag
```

Then add the snippet inside `<head>` of the app's HTML (for Vite/React/Lovable projects: `index.html` at the project root), ship it with `sb redeploy myapp --dir <path>`, run `sb wait myapp`, and verify with `sb check myapp --contains data-website-id`.

### Example: password manager

```sh
sb deploy vault --template vaultwarden
sb wait vault
sb settings vault signups=true      # exit code 3: give the user the approve URL
sb approvals --wait <id>            # once approved, the user registers at the app URL
sb settings vault signups=false     # after the account exists (no approval needed to close)
```

Signups close automatically 15 minutes after opening. To narrow who can register, also set `signup_domain=<company domain>` (for example `sb settings vault signups=true signup_domain=acme.com`).

Tell the user: in the Bitwarden app or browser extension, choose "Self-hosted" and enter the app URL.

## Data and storage

Templates that keep data (Vaultwarden, Umami's Postgres) use persistent volumes that survive restarts, redeploys and node failures. Deleting an app deletes its data. This prototype does not take backups yet, so tell the user not to store anything they cannot lose. `sb status <app>` shows a storage line.

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
