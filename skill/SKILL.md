---
name: slicedbread
description: Deploy and operate apps (off-the-shelf software, container images, git repos, local directories such as a Lovable export) on a SlicedBread tenant using the `sb` CLI. Use when asked to host, run, deploy, add analytics, or manage self-hosted apps.
---

# SlicedBread

SlicedBread runs apps on the user's own private cluster. Every app gets `https://<app>.<tenant-domain>` with a trusted certificate. Apps are **private by default**: a login wall in front of the app lets only the user's team in.

You operate it with the `sb` CLI. You never need kubectl, SSH or cloud consoles.

## Before you start

```sh
sb whoami      # confirms the CLI works; shows the tenant domain and the approvals URL
sb catalog     # off-the-shelf templates with their settings and actions
sb apps        # what is already running
```

If `sb whoami` fails with "not logged in", ask the user for the API URL and token, then: `echo "$TOKEN" | sb login --api https://api.<tenant-domain>`.

## Choosing how to deploy

| The user wants | Command |
|---|---|
| Known software (password manager, analytics) | `sb deploy <app> --template <name>` (check `sb catalog` first) |
| Their own code in a local folder (e.g. a Lovable export) | `sb deploy <app> --dir <path>` |
| A public git repo | `sb deploy <app> --repo https://github.com/org/repo` |
| A prebuilt container image | `sb deploy <app> --image <image> --port <port>` |

After every deploy, run `sb wait <app>`. It prints progress and ends with `ready: <url>` or an error with a hint. Builds from source take 2 to 5 minutes. Databases take 1 to 2 minutes to come up.

App names: 2 to 30 lowercase letters, digits or dashes, starting with a letter. `api`, `auth` and a few others are reserved.

### Source builds (`--dir`, `--repo`)

- Built in the cluster with Railpack (detects Node, Vite/React single-page apps, Python, Go and more). If the folder has a `Dockerfile`, it is used instead.
- The app must listen on `$PORT` (set to 8080). Railpack handles this for static sites and most frameworks. For a Dockerfile app that listens elsewhere, pass `--port`.
- `--dir` skips `node_modules`, `.git`, `dist`, `build` and similar folders. You do not need to build locally.
- To ship code changes: `sb redeploy <app> --dir <path>` (or `sb redeploy <app>` for a repo, which pulls the latest commit). The previous version keeps serving until the new build is ready.
- If a build fails: `sb logs <app> --build` shows the clone, plan and build output.

## Access modes

| Mode | Meaning |
|---|---|
| `wall` (default) | Visitors must sign in to SlicedBread first. Use for internal tools and dashboards. |
| `app-auth` | No SlicedBread wall; the app's own login protects it. Templates set this when needed (Vaultwarden, because Bitwarden apps cannot pass a browser login). |
| `public` | Anyone can reach it. |

Wall apps can expose specific paths publicly with `--public-path /path` (for webhooks or tracking scripts).

**Making an app reachable by more people needs a human.** Deploying with `--access public` or `app-auth`, adding public paths, changing a template setting that opens signups, and deleting an app all return **exit code 3** with a pending approval. When you see exit code 3:

1. Tell the user what you requested and give them the `approve at:` URL from the output.
2. Do not try to work around it (for example by redeploying under another name or with another mode).
3. Check progress with `sb approvals`. Once approved, the change applies automatically.

## Configuration

```sh
sb env <app>                                   # list keys (secret values are never shown)
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

Then add the snippet inside `<head>` of the app's HTML (for Vite/React/Lovable projects: `index.html` at the project root), and ship it: `sb redeploy myapp --dir <path>`. Verify by fetching the page HTML (for a wall app the page needs a login, so check the file you edited and that the redeploy finished with `sb wait`).

### Example: password manager

```sh
sb deploy vault --template vaultwarden
sb wait vault
sb settings vault signups=true      # exit code 3: ask the user to approve, then register at the app URL
sb settings vault signups=false     # after the account exists (no approval needed to close)
```

## Troubleshooting

| Symptom | Do this |
|---|---|
| `sb wait` shows `build-failed` | `sb logs <app> --build`, fix the source or add a Dockerfile, `sb redeploy <app> --dir <path>` |
| `starting` for several minutes | `sb status <app>` (message explains: image pull, crash, waiting for database), then `sb logs <app>` |
| `CrashLoopBackOff` / restarts | `sb logs <app>`; usually missing env or wrong port (`--port`) |
| `error: app ... already exists` | Use `sb redeploy`, `sb env`, or `sb settings`; deleting needs approval |
| Exit code 3 | Waiting for a human; see Access modes above |

## Rules

- Prefer templates over hand-rolled images when one exists.
- Default to `wall`. Only request wider access when the user asked for it or the template requires it.
- Report the app URL to the user when you finish, and say whether it is behind the login wall.
- Do not print or echo secret values.
- Use `--json` on any command if you need machine-readable output.
