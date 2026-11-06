# SlicedBread

Run off-the-shelf software and AI-built apps on infrastructure that keeps itself private, patched and recoverable.

> Status: pre-alpha. Design phase. Nothing here is ready to run yet.

## What it does

- **Deploy anything.** A catalog app (Vaultwarden, Umami, Metabase, n8n...), any container image, or a git repo that gets detected and built automatically.
- **Private by default.** Every app sits behind a login wall unless you say otherwise. Apps that need their own clients (like Vaultwarden) can opt out.
- **Recovers on its own.** If a node dies, apps move to a healthy one and their data comes with them. Backups run nightly and are restored and checked automatically.
- **Maintains itself.** OS patches, reboots and app updates happen without manual steps, with rollback when an update fails its health checks.
- **Works with your agent.** An MCP server, a CLI and an `AGENTS.md` let the coding agent you already use deploy and configure apps, for example "add analytics to my app with Umami".

## Docs

- [Architecture](docs/architecture.md)
- [App spec](docs/app-spec.md)
- [Technical decisions](docs/decisions.md)
- [Roadmap and acceptance tests](docs/roadmap.md)
- [Development and testing](docs/development.md)

## License

[AGPL-3.0](LICENSE)
