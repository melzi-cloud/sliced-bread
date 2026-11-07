# App spec (draft)

> Superseded for templates by [template-format.md](template-format.md) (D-032). Kept for the compose-like app spec ideas.

A compose-like YAML file with SlicedBread extensions. The control plane renders it into Kubernetes resources. Templates in the catalog and user apps use the same format.

The examples below are illustrative. Image tags, paths and health endpoints are placeholders until a template passes the verification workflow.

## Fields

| Field | Purpose |
|---|---|
| `name` | App name, also the subdomain: `<name>.<tenant>.slicedbread.cloud` |
| `services.<svc>.image` or `build` | Container image, or a source to build with Railpack |
| `services.<svc>.port` | Port the ingress routes to (one routed service per app for the MVP) |
| `services.<svc>.env` | Env vars. Supports references such as `${databases.main.url}`, `${secrets.<name>}`, `${apps.<other>.url}` |
| `services.<svc>.health` | HTTP or command health check, used for rollouts and restore checks |
| `services.<svc>.volumes` | Named volumes mounted into the service |
| `access.mode` | `wall` (default), `app-auth`, `public` |
| `access.public_paths` | Paths that bypass the wall when mode is `wall` |
| `databases.<name>` | Managed Postgres (CloudNativePG) |
| `volumes.<name>` | Persistent volume on Hetzner Volumes, with a size |
| `secrets.<name>` | Generated or user-provided secret |
| `backup` | Which volumes and databases to back up, plus a restore check |
| `actions.<name>` | Typed operations exposed through the API and MCP server |
| `hardening` | Required settings for `app-auth` apps, checked at deploy time |

## Example: Umami (wall with public tracking paths, typed action)

```yaml
name: analytics
services:
  web:
    image: ghcr.io/umami-software/umami:<pinned>
    port: 3000
    env:
      DATABASE_URL: ${databases.main.url}
      APP_SECRET: ${secrets.app_secret}
    health:
      http: /api/heartbeat        # verify
access:
  mode: wall
  public_paths: ["/script.js", "/api/send"]   # tracker must reach these without a login
databases:
  main: { type: postgres, version: "16" }
secrets:
  app_secret: { generate: 32 }
backup:
  databases: [main]
  restore_check: { http: /api/heartbeat }
actions:
  create_website:
    description: Create a tracked website and return the snippet to embed
    input:  { name: string, domain: string }
    output: { website_id: string, script_url: string, snippet: string }
```

## Example: Vaultwarden (app-auth with hardening)

```yaml
name: vault
services:
  web:
    image: vaultwarden/server:<pinned>
    port: 80
    env:
      DOMAIN: https://${app.host}
      SIGNUPS_ALLOWED: "false"
      # ADMIN_TOKEN unset: admin page disabled
    health:
      http: /alive                # verify
    volumes:
      data: /data
access:
  mode: app-auth                  # Bitwarden apps and extensions cannot pass a browser login wall
hardening:
  require_env: { SIGNUPS_ALLOWED: "false" }
  forbid_env: [ADMIN_TOKEN]
volumes:
  data: { size: 5Gi }
backup:
  volumes: [data]
  restore_check: { http: /alive }
```

## Example: a repo (Lovable export or any GitHub repo)

```yaml
name: crm
services:
  web:
    build: { repo: github.com/acme/crm, branch: main }   # Railpack detects the stack
    port: auto
    env:
      VITE_SUPABASE_URL: ${secrets.supabase_url}           # still served by Lovable Cloud
      VITE_SUPABASE_PUBLISHABLE_KEY: ${secrets.supabase_key}
access:
  mode: wall
```

Env var names for Lovable exports depend on the template Lovable used. Detect them from the repo instead of hardcoding.

## Template verification workflow

A template ships only when an agent-run workflow passes all steps on a scratch tenant:

1. Render and deploy.
2. Health check passes.
3. Log in (wall or app login) and perform a basic write.
4. Run each declared action and validate its output against the schema.
5. Back up, delete the app, restore, and confirm the written data is back.
6. Check that `app-auth` hardening rules hold and that only declared public paths bypass the wall.
