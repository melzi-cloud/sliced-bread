# Template format v1

Status: implemented (D-032, 2026-11-07). The built-in templates are in `internal/apps/templates/`; they move to `melzi-cloud/bakery` with the template registry (task #16). Builds on D-029 (actions are container jobs, multi-component apps come with the format) and D-030 (agents never see or write secret values).

## Goals

- A template is data, not code in sb-control. An agent or a contributor can write one, and CI can judge it (`verify.yaml`, task #13).
- One app can have several components (a web server, a worker, a cache), each with replicas, config files and routes.
- Everything an agent needs to operate the app is declared: settings it may change, actions it may run, what needs human approval, which values are secret, and what the human still has to do to finish setting the app up.
- Secrets never reach the agent. Generated secrets come from sb-control's random number generator.
- Prefer states over timers: nothing in a template depends on a human coming back within a time window.

Not in v1: extra hostnames per app (task #43), arbitrary Kubernetes objects, privileged containers, host paths, several databases per app, non-HTTP ports exposed to the internet, autoscaling (task #29).

## Layout

Templates live in `melzi-cloud/bakery`, one directory each:

```
bakery/
  umami/
    template.yaml        the template
    actions/             one script per action
      create-website.sh
      set-admin-password.sh
    files/               config file templates (if any)
    verify.yaml          what CI checks (task #13)
    README.md            for humans browsing the catalog
```

CI builds a signed index of all templates (minisign, task #16): names, versions, image digests and the content hash of every file. sb-control only deploys templates whose files match the index.

## Top-level fields

| Field | Required | Meaning |
|---|---|---|
| `apiVersion` | yes | `bakery.slicedbread.dev/v1` |
| `name` | yes | lowercase, also the default app name |
| `version` | yes | the template's own semver, bumped on any change; `sb update` (task #17) moves apps between versions |
| `description` | yes | one sentence for `sb catalog` |
| `upstream` | yes | `project`, `url`, `licence`, and `adaptedFrom` when based on a Coolify or Dokploy template (with its licence) |
| `access` | yes | default access, see below |
| `components` | yes | the containers |
| `routes` | no | which component serves which path; default: everything to the first component |
| `database` | no | `postgres: {version: "17"}` gives the app one CloudNativePG database |
| `secrets` | no | generated secrets and secrets a human provides |
| `settings` | no | typed options an agent may change |
| `forbidEnv` | no | env keys users and agents may not set |
| `backup` | no | how to back up volumes consistently |
| `actions` | no | operations agents or humans can run, and install steps |
| `setup` | no | what the human still has to do, shown on the dashboard until done |
| `notes` | no | guidance for agents, shown by `sb status` |
| `requires` | no | `sb: ">=0.4"`, the oldest sb-control that understands the template |

### References

Env values and config files may use references, resolved when sb-control renders the app:

| Reference | Value |
|---|---|
| `${app.url}` / `${app.host}` | `https://<app>.<domain>` / `<app>.<domain>` |
| `${db.url}` | Postgres connection URL (also `${db.host}`, `${db.user}`, `${db.password}`, `${db.name}`) |
| `${secret.NAME}` | a secret's value (generated or human-provided) |
| `${setting.NAME}` | a setting's current value |
| `${component.NAME.internal}` | `http://<component>.<app-namespace>.svc.cluster.local:<port>` |
| `${component.NAME.host}` / `${component.NAME.port}` | the same hostname and port separately, for configs that want them apart |
| `${apps.OTHER.url}` / `${apps.OTHER.internal}` | another app's URLs |

## access

```yaml
access:
  mode: app-auth          # wall | app-auth | public: the default for new apps
  publicPaths: [/script.js, /api/send]           # skip the login wall even in wall mode
  wallPaths: [/identity/accounts/register]       # need the login wall even in app-auth or public mode
```

`mode` is a default. If the user later narrows access (for example to `wall`), that choice is kept across settings changes and template updates; widening it needs approval as today. Paths match by prefix. `wallPaths` lets an app stay reachable for its own clients while a sensitive part (registration, an admin page) stays team-only.

## components

```yaml
components:
  - name: web                 # the first component gets the app's host by default
    image: ghcr.io/umami-software/umami:3.4.1@sha256:…   # tag for humans; digest required
    port: 3000
    health: {path: /api/heartbeat}
    replicas: 1               # fixed; 1 for components with a ReadWriteOnce volume
    resources: {cpu: 100m, memory: 256Mi}   # real numbers, checked by verify
    env:
      DATABASE_URL: ${db.url}
    volumes:
      - {name: data, path: /data, size: 10Gi}
    files:                    # config files, mounted read-only
      - {path: /config/config.yml, template: files/config.yml}
    command: [...]            # optional
    runAsUser: 1000           # optional; default is the image's user
```

Components without a port are workers. Every component gets today's hardening: no service account token, privilege escalation off, capabilities dropped, Pod Security baseline. Components reach each other with `${component.NAME.internal}`.

### routes

```yaml
routes:
  - {path: /, to: web}
  - {path: /api/v1/stream, to: companion}
```

All routes are on the app's host. Unlisted paths go to the first component.

## secrets

```yaml
secrets:
  generate:                   # created once by sb-control's random number generator
    - {name: APP_SECRET, bytes: 32, format: hex}      # format: hex | base64 | alnum
  provide:                    # entered by a human in the dashboard
    - {name: SMTP_PASSWORD, description: "Password of the mail account for invitations", required: false}
```

Agents only ever see secret names. A missing required `provide` secret shows as a setup step with a dashboard link. `sb env generate <app> KEY` generates a secret for apps not built from a template.

## settings

```yaml
settings:
  - name: signup_domain
    description: Only allow accounts with email addresses at this domain (empty: any).
    type: string               # enum (`values: [...]`), string (optional `pattern`), number, boolean
    pattern: '^([a-z0-9-]+\.)+[a-z]{2,}$'
    default: ""
    approvalFor: []            # values that make an agent's change wait for a human
    env:
      SIGNUPS_DOMAINS_WHITELIST: ${setting.signup_domain}
```

Numbers take optional `min` and `max`. A setting change re-renders the app; it never resets the access mode the user chose.

## backup

```yaml
backup:
  consistency: stop          # live (default) or stop (stop the app while copying, e.g. SQLite)
  require: [data/db.sqlite3] # restore tests fail if missing or empty
  exclude: [data/icon_cache]
```

The database is always backed up with `pg_dump` and restore-tested; volumes unless excluded.

## actions

An action is a script that runs as a Job next to the app (D-029).

```yaml
actions:
  - name: create_website
    description: Register a website and return the tracking snippet for its <head>.
    image: ghcr.io/melzi-cloud/bakery-runtime:1.0.0@sha256:…   # every action names its image
    run: actions/create-website.sh
    secrets: [UMAMI_ADMIN_PASSWORD]     # the only secrets the job receives
    input:
      name: {type: string, required: true, description: display name}
      domain: {type: string, required: true, description: "site domain, e.g. shop.example.com"}
    output:
      website_id: {type: string}
      snippet: {type: string}
  - name: dashboard_login
    image: ghcr.io/melzi-cloud/bakery-runtime:1.0.0@sha256:…
    run: actions/dashboard-login.sh
    secrets: [UMAMI_ADMIN_PASSWORD]
    output:
      username: {type: string}
      password: {type: string, secret: true}   # only shown to a signed-in human in the dashboard
  - name: set_admin_password
    on: install                          # runs once the app is first healthy; blocks until it succeeds
    image: ghcr.io/melzi-cloud/bakery-runtime:1.0.0@sha256:…
    run: actions/set-admin-password.sh
    secrets: [UMAMI_ADMIN_PASSWORD]
```

**How an action runs:**
- **Image:** every action names one, pinned by digest: the app's own image when the action needs its CLI, a Postgres image for `psql`, or a general one. The built-in templates use the official `node` image, which has `fetch` and SQLite built in.
- **Files:** the template directory is mounted at `/sb/template` (the working directory); `run` names the script, which must start with `#!`. Volumes listed in `volumes` are mounted read-only at `/volumes/<name>`.
- **Logs:** `sb logs <app> --action <name>` shows the latest run (kept 15 minutes), with secret values masked.
- **Environment:** `SB_APP_INTERNAL` (the first component's internal URL), `SB_APP_URL`, each input as `SB_INPUT_<NAME>`, `SB_DB_URL` if the action lists `database: true`, and only the declared secrets.
- **Volumes:** `volumes: [data]` mounts the app's volume read-only (the job runs on the app's server).
- **Network:** the job reaches the app, its database and DNS only, unless the action lists `egress: [hosts]`.
- **Result:** the script prints one JSON object as its last line, checked against `output`. A non-zero exit fails the action with the last stderr line as the message. Time limit 60 seconds unless `timeout:` says otherwise.
- **Secrets in results:** any secret value of the app in the output or error is replaced with `[secret]`. Fields marked `secret: true` never go to the API; an agent gets a dashboard link for the human instead (D-030).
- **Triggers:** `on: install` blocks: the app shows `configuring` until it succeeds (retried with backoff, error shown in `sb status`). `on: upgrade` runs after `sb update` (task #17). Default is manual. `approval: true` makes an agent's call wait for a human.

## setup

Steps the human has to do after deploying, with a way to tell when each is done. The dashboard shows open steps on the app page and the Apps list; `sb status` shows them to agents. A step disappears by itself when its check passes: no timers, no "finish" button.

```yaml
setup:
  - name: first_account
    description: Create your Vaultwarden account.
    link: /#/signup              # opened through the login wall first, so wallPaths work
    doneWhen: has_account        # an action returning {"done": true}; checked every few minutes until done
```

The link goes through `https://<app>.<domain>/__sb/enter?to=/#/signup`: sb-control asks for the login wall (setting the app host's session cookie), then redirects. This is what lets the browser pass `wallPaths` on an app that is otherwise `app-auth`.

## Full examples

### Vaultwarden

Registration is team-only, permanently: signups are on inside Vaultwarden, but the registration endpoints need the login wall, so only members of the install can create an account. Bitwarden apps and extensions still log in without the wall. Verified on Vaultwarden 1.37.5 (web vault 2026.10.1, 2026-11-07): the web vault registers through `/identity/accounts/register/send-verification-email` and `/identity/accounts/register/finish`; a browser that passed the wall created an account; without a session all registration endpoints, including the older `/api/accounts/register`, answered 401.

```yaml
apiVersion: bakery.slicedbread.dev/v1
name: vaultwarden
version: 1.0.0
description: Password manager compatible with Bitwarden apps and browser extensions.
upstream: {project: Vaultwarden, url: https://github.com/dani-garcia/vaultwarden, licence: AGPL-3.0}
access:
  mode: app-auth               # Bitwarden clients cannot pass the login wall
  wallPaths: [/identity/accounts/register, /api/accounts/register]
components:
  - name: web
    image: vaultwarden/server:1.37.5@sha256:…
    port: 8080
    health: {path: /alive}
    resources: {cpu: 50m, memory: 128Mi}
    env:
      ROCKET_PORT: "8080"
      DOMAIN: ${app.url}
      SIGNUPS_ALLOWED: "true"   # safe: registration is behind the login wall
      INVITATIONS_ALLOWED: "false"
      SHOW_PASSWORD_HINT: "false"
      DATABASE_CONN_INIT: "PRAGMA busy_timeout = 5000; PRAGMA synchronous = FULL;"
    volumes: [{name: data, path: /data, size: 10Gi}]
settings:
  - name: signup_domain
    description: Only allow accounts with email addresses at this domain (empty: any member of the install).
    type: string
    default: ""
    env: {SIGNUPS_DOMAINS_WHITELIST: "${setting.signup_domain}"}
forbidEnv: [ADMIN_TOKEN, SIGNUPS_ALLOWED, SIGNUPS_DOMAINS_WHITELIST]
backup: {consistency: stop, require: [data/db.sqlite3]}
actions:
  - name: has_account
    description: Whether at least one account exists.
    image: docker.io/keinos/sqlite3:3.54.1@sha256:…
    volumes: [data]
    run: actions/has-account.sh       # sqlite3 -readonly /data/db.sqlite3 'select count(*) from users'
    output: {done: {type: boolean}, accounts: {type: number}}
setup:
  - name: first_account
    description: Create your Vaultwarden account (members of this install can create one at any time).
    link: /#/signup
    doneWhen: has_account
notes: |
  Only members of this install can create accounts: give them the account link from `sb status`
  (it passes the login wall first). Clients: in the Bitwarden app or extension choose Self-hosted
  and enter the app URL.
```

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
actions: [set_admin_password (on install), create_website, test_event, stats, dashboard_login]   # as in the actions section
notes: |
  The dashboard sits behind the login wall and has its own Umami login (dashboard_login, shown to humans).
  Use create_website to get a snippet; verify with test_event and read counts with stats.
```

Example action script (`actions/create-website.sh`):

```sh
set -eu
tok=$(curl -fsS "$SB_APP_INTERNAL/api/auth/login" -H 'content-type: application/json' \
  -d "$(jq -n --arg p "$UMAMI_ADMIN_PASSWORD" '{username:"admin",password:$p}')" | jq -r .token)
site=$(curl -fsS "$SB_APP_INTERNAL/api/websites" -H "authorization: Bearer $tok" \
  -H 'content-type: application/json' \
  -d "$(jq -n --arg n "$SB_INPUT_NAME" --arg d "$SB_INPUT_DOMAIN" '{name:$n,domain:$d}')")
id=$(echo "$site" | jq -r .id)
jq -cn --arg id "$id" --arg url "$SB_APP_URL/script.js" \
  '{website_id: $id, snippet: "<script defer src=\"\($url)\" data-website-id=\"\($id)\"></script>"}'
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
    env: {SERVER_SECRET_KEY: "${secret.COMPANION_KEY}"}
# files/config.yml tells web where the companion is:
#   invidious_companion:
#     - private_url: "${component.companion.internal}/companion"
#   invidious_companion_key: "${secret.COMPANION_KEY}"
routes:
  - {path: /, to: web}
  - {path: /latest_version, to: companion}
  - {path: /api/manifest/dash/id, to: companion}
database: {postgres: {version: "17"}}
secrets:
  generate: [{name: COMPANION_KEY, bytes: 16, format: alnum}, {name: HMAC_KEY, bytes: 32, format: hex}]
```

## What sb-control does

- Renders components, routes, the database, `publicPaths` and `wallPaths`, and network policies.
- Generates secrets, collects provided ones through the dashboard, masks action output.
- Runs actions as Jobs, checks their output, runs `on: install` actions and `setup` checks.
- Serves `/__sb/enter` on app hosts.

## Decisions taken in review (Antonio, 2026-11-07)

1. Images are pinned by digest in every template.
2. Every action names its own image; a small runtime image is published as a convenience.
3. Extra hostnames per app are not in v1 (task #43).
4. `on: install` actions block: the app is `configuring` until they succeed.
5. Settings types: enum, string (with optional pattern), number, boolean.
6. Replicas are fixed numbers; autoscaling later.
7. No time-based behaviour: Vaultwarden's 15-minute signup window is replaced by team-only registration (`wallPaths`) and a setup step that completes itself.
8. The index is signed with minisign.
