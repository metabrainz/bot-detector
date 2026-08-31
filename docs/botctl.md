# botctl — command-line client

`botctl` is a small CLI for interacting with a running bot-detector instance
over its HTTP API, instead of crafting `curl` calls by hand. Each command maps
to exactly one API endpoint (see [API.md](API.md)).

The most common workflow — check whether an IP is blocked and unblock it if so
— is built in: `botctl ip unblock <ip>` checks status first and only unblocks a
blocked IP (after confirmation).

## Building

```sh
go build -o botctl ./cmd/botctl
```

## Target selection

The target instance is chosen in this priority order:

1. `--url <url>` flag
2. `BOT_DETECTOR_URL` environment variable
3. Default: `http://localhost:8090`

The URL must point at a listener serving the relevant roles (`api`, `metrics`,
`cluster`). See the `--listen` documentation in the [README](../README.md).

```sh
export BOT_DETECTOR_URL=http://gateway1:8090
botctl ip check 1.2.3.4
```

## Global flags

| Flag | Description |
| :--- | :--- |
| `--url <url>` | Base URL of the bot-detector API. |
| `--json` | Print the raw JSON/body from the server instead of a formatted summary. |
| `-y`, `--yes` | Skip the confirmation prompt for destructive actions. |
| `--timeout <dur>` | HTTP timeout (default `10s`, e.g. `5s`, `1m`). |
| `-h`, `--help` | Show usage. |

## Commands

Each command maps to a single API endpoint.

### IP operations

| Command | Endpoint | Description |
| :--- | :--- | :--- |
| `botctl ip check <ip>` | `GET /api/v1/ip/{ip}` | Show block status of an IP. |
| `botctl ip unblock <ip>` | `POST /api/v1/ip/{ip}/unblock` | Unblock an IP (checks status first). |
| `botctl ip clear <ip>` | `POST /api/v1/ip/{ip}/clear` | Clear an IP from all state. |

`ip check` uses distinct exit codes so scripts can branch without parsing
output:

| Exit code | Meaning |
| :--- | :--- |
| `0` | Not blocked |
| `2` | Blocked (including cluster `mixed`, or blocked on any node) |
| `3` | Unknown to the instance |
| `1` | Usage / network / server error |

`ip unblock` and `ip clear` are destructive and prompt for confirmation unless
`--yes` is given. `ip unblock` first checks status and does nothing if the IP is
not blocked.

### Bulk block operations

| Command | Endpoint | Description |
| :--- | :--- | :--- |
| `botctl blocks unblock --reason <r>` | `POST /api/v1/blocks/unblock?reason=` | Unblock all IPs currently blocked by a reason substring. |

### Bad actors

| Command | Endpoint | Description |
| :--- | :--- | :--- |
| `botctl bad-actors list [--reason <r>]` | `GET /api/v1/bad-actors` | List bad actors, optionally filtered by reason substring (client-side). |
| `botctl bad-actors stats` | `GET /api/v1/bad-actors/stats` | Counts by reason and by day. |
| `botctl bad-actors export` | `GET /api/v1/bad-actors/export` | Bad actor IPs, one per line. |
| `botctl bad-actors remove --reason <r> [--unblock]` | `DELETE /api/v1/bad-actors?reason=` | Remove bad actors by reason; `--unblock` also unblocks them. |

The `--reason` match is a **substring** match, consistent with the server.

### Configuration

| Command | Endpoint | Description |
| :--- | :--- | :--- |
| `botctl config show` | `GET /config` | Print the running YAML configuration. |
| `botctl config archive [-o <file>]` | `GET /config/archive` | Download config + dependencies as a `.tar.gz` (default `bot-detector-config.tar.gz`). |

### Metrics and cluster

| Command | Endpoint | Description |
| :--- | :--- | :--- |
| `botctl metrics show [--aggregate]` | `GET /api/v1/cluster/metrics[/aggregate]` | Node metrics, or cluster-wide with `--aggregate` (leader only). |
| `botctl cluster status` | `GET /api/v1/cluster/status` | This node's role, name, and address. |
| `botctl cluster state [--reason <r>]` | `GET /api/v1/cluster/state/merged` | Merged cluster block state, optionally filtered by reason. |
| `botctl endpoints` | `GET /api/v1/help` | List the instance's API endpoints. |

> The internal `/api/v1/cluster/internal/*` endpoints (leader↔follower
> plumbing) are intentionally **not** exposed by `botctl`, because calling them
> directly can desync cluster state. Use the cluster-aware public commands
> above; the leader broadcasts changes to followers automatically.

## Examples

```sh
# Is this IP blocked? (exit code reflects the answer)
botctl ip check 1.2.3.4

# Check and unblock if blocked, on a specific gateway
BOT_DETECTOR_URL=http://gateway1:8090 botctl ip unblock 1.2.3.4

# List bad actors promoted by a specific chain
botctl bad-actors list --reason abusers-444

# Remove and unblock all bad actors from an overzealous chain, no prompt
botctl bad-actors remove --reason abusers-444 --unblock --yes

# Raw JSON for scripting
botctl --json cluster status
```
