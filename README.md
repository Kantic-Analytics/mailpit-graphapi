# mailpit-graphapi

`mailpit-graphapi` is a small Microsoft Graph Mail compatibility sidecar for
[Mailpit](https://github.com/axllent/mailpit). It lets an application exercise its
Graph-based mail workflow locally without connecting to Microsoft 365 or sending
email to the internet.

> [!IMPORTANT]
> This is a test double, not a Microsoft Graph proxy. It implements a documented
> subset of Graph Mail semantics and must not be exposed to an untrusted network.

## What it does

- issues a local OAuth `client_credentials` token;
- lists Inbox, Drafts and Sent Items messages;
- exposes message metadata and raw RFC 822 content;
- maps Graph `isRead` and `categories` to Mailpit read state and tags;
- simulates message moves between Inbox, Drafts and Sent Items;
- simulates configurable application folders through Mailpit tags;
- stores Graph state only in readable ASCII tags such as
  `Categorie - A traiter par un humain` and `Dossier - SAV`;
- captures Graph `sendMail`, threaded reply calls and draft creation in Mailpit;
- never contacts Microsoft Graph and never relays mail itself.

Mailpit does not currently expose an in-process plugin API. This project is
therefore a real, independently versioned sidecar: the Graph-compatible HTTP API
runs next to Mailpit and uses Mailpit's supported HTTP API as its storage adapter.

## Quick start

Prerequisites: Go 1.23+ and Mailpit.

```sh
mailpit --listen 127.0.0.1:8025 --smtp 127.0.0.1:1025
go run ./cmd/mailpit-graphapi
```

Defaults:

| Setting | Default |
|---|---|
| Graph-compatible API | `http://127.0.0.1:8081` |
| Mailpit API | `http://127.0.0.1:8025` |
| Local bearer token | `mailpit-graphapi-local` |

Request a token:

```sh
curl -sS -X POST http://127.0.0.1:8081/local/oauth2/v2.0/token \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data 'grant_type=client_credentials&scope=https%3A%2F%2Fgraph.microsoft.com%2F.default'
```

Configure a Graph client with:

- token URL: `http://127.0.0.1:8081/local/oauth2/v2.0/token`
- Graph base URL: `http://127.0.0.1:8081/v1.0`
- tenant/client credentials: arbitrary values unless validation is enabled

## Configuration

Flags have equivalent environment variables:

| Flag | Environment variable | Purpose |
|---|---|---|
| `--listen` | `MAILPIT_GRAPH_LISTEN` | listen address |
| `--mailpit-url` | `MAILPIT_URL` | Mailpit HTTP base URL |
| `--token` | `MAILPIT_GRAPH_TOKEN` | bearer token returned by local OAuth |
| `--client-id` | `MAILPIT_GRAPH_CLIENT_ID` | optional required client ID |
| `--client-secret` | `MAILPIT_GRAPH_CLIENT_SECRET` | optional required client secret |
| `--folders` | `MAILPIT_GRAPH_FOLDERS` | comma-separated application folders |

Remote Mailpit URLs are rejected by default. For an isolated container network,
pass `--allow-remote-mailpit` explicitly.

## Supported Graph routes

See [docs/compatibility.md](docs/compatibility.md) for exact routes, mappings and
known differences from Microsoft Graph.

## Docker

```sh
docker compose -f examples/compose.yaml up --build
```

The compose example publishes both web interfaces on loopback only. SMTP is also
loopback-only and Mailpit relay is not configured.

## Development

```sh
make check
```

The project uses only the Go standard library at runtime. Releases are intended
to ship as a single binary.

## Security

Read [SECURITY.md](SECURITY.md) before deploying. This software is deliberately
designed for local development and acceptance testing, not production traffic.

## License

MIT — see [LICENSE](LICENSE).
