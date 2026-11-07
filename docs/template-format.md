# Template format v1 (draft for review)

Status: proposal, not implemented. Replaces the Go templates in `internal/apps/catalog.go` and the Go actions in `internal/control/hooks.go` once agreed. Decisions it builds on: D-029 (actions are container jobs, multi-component apps come with the format) and D-030 (agents never see or write secret values).

## Goals

- A template is data, not code in sb-control. An agent or a contributor can write one, and CI can judge it (`verify.yaml`, task #13).
- One app can have several components (a web server, a worker, a cache), each with replicas, config files and routes.
- Everything an agent needs to operate the app is declared: settings it may change, actions it may run, what needs human approval, which values are secret.
- Secrets never reach the agent. Generated secrets come from sb-control's random number generator.

Not in v1: arbitrary Kubernetes objects, privileged containers, host paths, several databases per app, non-HTTP ports exposed to the internet.

## Layout

Templates live in the `melzi-cloud/bakery` repository, one directory each:

```
bakery/
  umami/
    template.yaml        the template
    actions/             scripts for actions (one file per action)
      create-website.sh
      stats.sh
      set-admin-password.sh
    verify.yaml          what CI checks (task #13)
    README.md            for humans browsing the catalog
```

`template.yaml` is the only file sb-control reads at deploy time. Action scripts are bundled into the published index with their content hash.

## Top-level fields

| Field | Required | Meaning |
|---|---|---|
| `apiVersion` | yes | `bakery.slicedbread.dev/v1` |
| `name` | yes | lowercase, also the default app name |
| `version` | yes | the template's own version (semver). Bumped on any change; `sb update` (task #17) moves apps between versions |
| `description` | yes | one sentence for `sb catalog` |
| `upstream` | yes | `project`, `url`, `licence`, and `adaptedFrom` when based on a Coolify or Dokploy template (with its licence) |
| `access` | yes | `mode: wall \| app-auth \| public` and `publicPaths` (paths that skip the login wall even in `wall` mode) |
| `components` | yes | the containers, see below |
| `routes` | no | which component serves which path; default: everything to the first component |
| `database` | no | `postgres: {version: "17"}` gives the app one CloudNativePG database |
| `secrets` | no | generated secrets and secrets a human provides |
| `settings` | no | typed options an agent may change, mapped to env |
| `forbidEnv` | no | env keys users and agents may not set (they are managed by settings or would weaken the app) |
| `backup` | no | how to back up volumes consistently |
| `actions` | no | operations agents (or humans) can run |
| `notes` | no | guidance for agents, shown by `sb status` |
| `requires` | no | `sb: ">=0.4"`, the oldest sb-control that understands this template |

### Values and references

Env values and config files may use references, resolved by sb-control when it renders the app:

| Reference | Value |
|---|---|
| `${app.url}` / `${app.host}` | `https://<app>.<domain>` / `<app>.<domain>` |
| `${db.url}` | Postgres connection URL (also `${db.host}`, `${db.user}`, `${db.password}`, `${db.name}`) |
| `${secret.NAME}` | a secret's value (generated or human-provided) |
| `${setting.NAME}` | a setting's current value |
| `${component.NAME.internal}` | `http://<component>.<app-namespace>.svc.cluster.local:<port>` |
| `${apps.OTHER.url}` / `${apps.OTHER.internal}` | another app's URLs (as today) |

## components

```yaml
components:
  - name: web                 # first component gets the app's host by default
    image: ghcr.io/umami-software/umami:3.4.1@sha256:…   # tag for humans, digest is what runs
    port: 3000
    health: {path: /api/heartbeat}
    replicas: 1               # fixed; components with a ReadWriteOnce volume must stay at 1
    resources: {cpu: 100m, memory: 256Mi}   # real numbers, checked by verify
    env:
      DATABASE_URL: ${db.url}
      APP_SECRET: ${secret.APP_SECRET}
    volumes:
      - {name: data, path: /data, size: 10Gi}
    files:                    # config files mounted read-only
      - path: /config/config.yml
        template: files/config.yml   # rendered with the references above
    command: [...]            # optional, overrides the image's
    runAsUser: 1000           # optional; default is the image's user
```

Components without a port are workers (no Service). Every component gets the same hardening as apps today: no service account token, privilege escalation off, capabilities dropped, Pod Security baseline. A component reaches another one with `${component.NAME.internal}`; the app's network policy already allows traffic inside the app.

### routes

```yaml
routes:
  - {path: /, to: web}
  - {path: /api/v1/stream, to: companion}
```

All routes are on the app's host. Paths not listed go to the first component. Extra hosts (for example `api.<app>.<domain>`) are left for v2 unless a launch template needs them.

## secrets

```yaml
secrets:
  generate:                   # created once by sb-control's random number generator
    - {name: APP_SECRET, bytes: 32, format: hex}
    - {name: UMAMI_ADMIN_PASSWORD, bytes: 24, format: alnum}
  provide:                    # entered by a human in the dashboard (secret request link)
    - {name: SMTP_PASSWORD, description: "Password of the mail account used for invitations", required: false}
```

Agents only ever see secret names. `sb env generate <app> KEY` does the same for apps not built from a template.

## settings

```yaml
settings:
  - name: signups
    description: Allow new accounts to register.
    values: ["false", "true"]        # or `type: string` with an optional `pattern`
    default: "false"
    approvalFor: ["true"]            # an agent setting this value waits for a human
    autoRevert: {after: 15m, to: "false"}   # sb-control turns it back by itself
    env:
      SIGNUPS_ALLOWED: ${setting.signups}
  - name: signup_domain
    description: Only allow signups with email addresses at this domain (empty: any).
    type: string
    pattern: '^([a-z0-9-]+\.)+[a-z]{2,}$'
    default: ""
    env:
      SIGNUPS_DOMAINS_WHITELIST: ${setting.signup_domain}
```

`autoRevert` replaces the Go code that closes Vaultwarden signups today; it is the one timer sb-control keeps running for templates.

## backup

```yaml
backup:
  consistency: stop          # live (default): copy while running; stop: stop the app first (SQLite)
  require: [data/db.sqlite3] # restore tests fail if these are missing or empty
  exclude: [data/icon_cache]
```

The database is always backed up with `pg_dump` and restore-tested. Volumes are backed up unless excluded.

## actions

An action is a script that runs as a Job next to the app (D-029).

```yaml
actions:
  - name: create_website
    description: Register a website and return the tracking snippet for its <head>.
    run: actions/create-website.sh
    secrets: [UMAMI_ADMIN_PASSWORD]     # the only secrets the job receives
    input:
      name: {type: string, required: true, description: display name}
      domain: {type: string, required: true, description: "site domain, e.g. shop.example.com"}
    output:
      website_id: {type: string}
      snippet: {type: string}
      next: {type: string}
  - name: dashboard_login
    description: The Umami dashboard username and password.
    run: actions/dashboard-login.sh
    secrets: [UMAMI_ADMIN_PASSWORD]
    output:
      username: {type: string}
      password: {type: string, secret: true}   # only shown to a signed-in human in the dashboard
  - name: set_admin_password
    description: Replace Umami's default admin password with the generated one.
    on: install                          # runs by itself once the app is first healthy
    run: actions/set-admin-password.sh
    secrets: [UMAMI_ADMIN_PASSWORD]
```

**How an action runs:**
- **Image:** a small runtime image we publish (`sh`, `curl`, `jq`, CA certificates), or `image:` per action, for example the app's own image when the action needs its CLI.
- **Environment:** `SB_APP_INTERNAL` (the first component's internal URL), `SB_APP_URL`, each input as `SB_INPUT_<NAME>`, and only the declared secrets.
- **Network:** the job can reach the app and DNS, nothing else, unless the action lists `egress: [hosts]`.
- **Result:** the script prints one JSON object as its last line; sb-control checks it against `output`. A non-zero exit fails the action with the script's last stderr line as the message. Time limit 60 seconds unless `timeout:` says otherwise.
- **Secrets in results:** any secret value of the app found in the output or the error is replaced with `[secret]` before anything is returned. Fields marked `secret: true` are never returned to the API; an agent gets a link for the human instead (D-030).
- **Triggers:** `on: install` (once, the app shows `configuring` until it succeeds, retried with backoff), `on: upgrade` (after `sb update`, task #17), or manual (default). `approval: true` makes an agent's call wait for a human.

Example script (`actions/create-website.sh`):

```sh
set -eu
tok=$(curl -fsS "$SB_APP_INTERNAL/api/auth/login" -H 'content-type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"$UMAMI_ADMIN_PASSWORD\"}" | jq -r .token)
site=$(curl -fsS "$SB_APP_INTERNAL/api/websites" -H "authorization: Bearer $tok" \
  -H 'content-type: application/json' \
  -d "$(jq -n --arg n "$SB_INPUT_NAME" --arg d "$SB_INPUT_DOMAIN" '{name:$n,domain:$d}')")
id=$(echo "$site" | jq -r .id)
jq -cn --arg id "$id" --arg url "$SB_APP_URL/script.js" '{
  website_id: $id,
  snippet: "<script defer src=\"\($url)\" data-website-id=\"\($id)\"></script>",
  next: "Add the snippet inside <head> of the site, then redeploy that app."}'
```

## Full examples

### Umami

```yaml
apiVersion: bakery.slicedbread.dev/v1
name: umami
version: 1.0.0
description: Privacy-friendly web analytics with a Postgres database.
upstream: {project: Umami, url: https://umami.is, licence: MIT}
access:
  mode: wall
  publicPaths: [/script.js, /api/send]
components:
  - name: web
    image: ghcr.io/umami-software/umami:3.4.1@sha256:…
    port: 3000
    health: {path: /api/heartbeat}
    resources: {cpu: 100m, memory: 384Mi}
    env:
      DATABASE_URL: ${db.url}
      APP_SECRET: ${secret.APP_SECRET}
database: {postgres: {version: "17"}}
secrets:
  generate:
    - {name: APP_SECRET, bytes: 32, format: hex}
    - {name: UMAMI_ADMIN_PASSWORD, bytes: 24, format: alnum}
actions: [set_admin_password (on install), create_website, test_event, stats, dashboard_login]  # as above
notes: |
  The dashboard sits behind the login wall and has its own Umami login (dashboard_login, shown to humans).
  Use create_website to get a snippet; verify with test_event and read counts with stats.
```

### Vaultwarden

```yaml
apiVersion: bakery.slicedbread.dev/v1
name: vaultwarden
version: 1.0.0
description: Password manager compatible with Bitwarden apps and browser extensions.
upstream: {project: Vaultwarden, url: https://github.com/dani-garcia/vaultwarden, licence: AGPL-3.0}
access: {mode: app-auth}     # Bitwarden clients cannot pass the login wall
components:
  - name: web
    image: vaultwarden/server:1.37.5@sha256:…
    port: 8080
    health: {path: /alive}
    resources: {cpu: 50m, memory: 128Mi}
    env:
      ROCKET_PORT: "8080"
      DOMAIN: ${app.url}
      INVITATIONS_ALLOWED: "false"
      SHOW_PASSWORD_HINT: "false"
      DATABASE_CONN_INIT: "PRAGMA busy_timeout = 5000; PRAGMA synchronous = FULL;"
    volumes: [{name: data, path: /data, size: 10Gi}]
settings: [signups, signup_domain]   # as in the settings section
forbidEnv: [ADMIN_TOKEN, SIGNUPS_ALLOWED, SIGNUPS_DOMAINS_WHITELIST]
backup: {consistency: stop, require: [data/db.sqlite3]}
notes: |
  Signups are off by default. To create the first account: set signups=true (needs approval),
  the user registers at the app URL; signups close by themselves after 15 minutes.
```

### Multi-component sketch: Invidious

```yaml
components:
  - name: web
    image: quay.io/invidious/invidious:…@sha256:…
    port: 3000
    files: [{path: /invidious/config/config.yml, template: files/config.yml}]
  - name: companion
    image: quay.io/invidious/invidious-companion:…@sha256:…
    port: 8282
    env: {SERVER_SECRET_KEY: ${secret.COMPANION_KEY}}
routes:
  - {path: /, to: web}
  - {path: /latest_version, to: companion}
  - {path: /api/manifest/dash/id, to: companion}
database: {postgres: {version: "17"}}
secrets:
  generate: [{name: COMPANION_KEY, bytes: 16, format: alnum}, {name: HMAC_KEY, bytes: 32, format: hex}]
```

## What stays in sb-control

- Rendering components, routes, the database and network policies (as today, generalised to several components).
- Generating secrets, masking outputs, the dashboard pages for secret requests and secret outputs.
- Running actions as Jobs and checking their output.
- `autoRevert` timers.

## Open questions for review

1. **Images by digest:** require `@sha256:` in every template (my lean: yes, CI fills it in), or allow tags for community templates?
2. **Action runtime:** one published runtime image with `sh`, `curl` and `jq` (lean), or let every action pick its image?
3. **Extra hosts:** leave out of v1 (lean), or support a second host now because a launch template needs it?
4. **Install actions blocking:** an app stays `configuring` until its `on: install` actions succeed (lean), or `running` with a warning?
5. **Settings types:** enum and string with a pattern (lean), or also numbers and booleans?
6. **Per-component replicas:** fixed numbers only (lean), or ranges for later autoscaling (task #29)?
