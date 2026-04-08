# Bad Actors

Bot-detector tracks IPs that are blocked repeatedly across chains and websites. When an IP's cumulative score reaches a configurable threshold, it is promoted to "bad actor" status and blocked for an extended duration.

## How It Works

Each behavioral chain has a `bad_actor_weight` (0.0–1.0, default 1.0). Every time a chain completes and blocks an IP, the weight is added to that IP's score. When the score reaches the configured threshold, the IP is promoted to a bad actor.

**Scoring examples:**

| Scenario | Blocks | Score | Result |
|:---|:---|:---|:---|
| 5× SQL-Injection (weight 1.0) | 5 | 5.0 | Bad actor (threshold 5.0) |
| 2× SQL-Injection (1.0) + 10× Suspicious-UA (0.3) | 12 | 5.0 | Bad actor |
| 10× Suspicious-UA (weight 0.3) | 10 | 3.0 | Not yet |

## Configuration

```yaml
bad_actors:
  enabled: true
  threshold: 5.0             # Score needed to become a bad actor
  block_duration: "168h"     # Block duration for bad actors (default: 1 week)
  max_score_entries: 100000  # Max IPs tracked in scoring table
  score_max_age: "30d"       # Remove low scores older than this (default: 30 days)
  score_min_cleanup: 2.0     # Only remove scores below this value (default: 2.0)

chains:
  - name: "SQL-Injection"
    action: block
    block_duration: "1h"
    bad_actor_weight: 1.0   # Default if not specified
    match_key: ip
    steps: [...]

  - name: "Suspicious-User-Agent"
    action: block
    block_duration: "30m"
    bad_actor_weight: 0.3   # Lower weight for less severe chains
    match_key: ip
    steps: [...]
```

### Configuration Fields

| Field | Type | Default | Description |
|:---|:---|:---|:---|
| `bad_actors.enabled` | bool | `true` (if section present) | Enable/disable bad actor tracking |
| `bad_actors.threshold` | float | required | Score needed for promotion |
| `bad_actors.block_duration` | duration | `168h` | How long to block bad actors |
| `bad_actors.max_score_entries` | int | `100000` | Max IPs in scoring table |
| `chains[].bad_actor_weight` | float | `1.0` | Weight added to score per block (0.0–1.0) |

If the `bad_actors` section is absent from the config, the feature is disabled.

## Processing Priority

When processing a log entry:

1. **Good actors** (from config) — skip all processing
2. **Bad actors** (from database) — skip chain processing, ensure blocked
3. **Normal processing** — run behavioral chains

Good actors always take priority over bad actors. If an IP is in both `good_actors` config and the `bad_actors` database, the bad actor entry is removed on the next `DELETE /ip/{ip}/clear`.

## Promotion

When an IP's score reaches the threshold:

1. The IP is inserted into the `bad_actors` table with a JSON history of recent block events
2. A max-duration block command is issued to HAProxy
3. The event is logged at CRITICAL level:
   ```
   BAD_ACTOR: IP 1.2.3.4 promoted to bad actor (score=5.0, blocks=5, chain=SQL-Injection, weight=1.0)
   ```

Bad actors are **permanent** — they are never automatically cleaned up. They must be manually removed via the API.

## Removal

Use the existing clear endpoint:

```
DELETE /ip/{ip}/clear
```

This removes the IP from:
- `bad_actors` table
- `ip_scores` table
- `ips` table
- HAProxy stick tables

The endpoint is cluster-aware and broadcasts to all nodes.

## API Endpoints

### GET /api/v1/bad-actors

Returns all bad actors as JSON:

```json
[
  {
    "ip": "1.2.3.4",
    "promoted_at": "2026-03-12T16:00:00Z",
    "total_score": 5.5,
    "block_count": 7,
    "history": "[{\"ts\":\"2026-03-12T16:00:00Z\",\"r\":\"SQL-Injection\"},...]"
  }
]
```

### GET /api/v1/bad-actors/export

Returns bad actor IPs as plain text, one per line:

```
1.2.3.4
5.6.7.8
```

Useful for integration with external firewalls or blocklists.

## Database Schema

Two tables are added via schema migration v2 (timestamps converted to Unix seconds in v4):

**ip_scores** — tracks cumulative score per IP (cleaned up periodically):
```sql
CREATE TABLE ip_scores (
    ip TEXT PRIMARY KEY,
    score REAL NOT NULL DEFAULT 0.0,
    block_count INTEGER NOT NULL DEFAULT 0,
    last_block_time INTEGER NOT NULL  -- Unix seconds
);
```

**bad_actors** — permanent bad actor records:
```sql
CREATE TABLE bad_actors (
    ip TEXT PRIMARY KEY,
    promoted_at INTEGER NOT NULL,  -- Unix seconds
    total_score REAL NOT NULL,
    block_count INTEGER NOT NULL,
    history_json TEXT
);
```

## Cleanup

During periodic cleanup (configured via `compaction_interval`):
- Scores below `score_min_cleanup` (default: 2.0) older than `score_max_age` (default: 30 days) are removed from `ip_scores`
- Bad actors are **never** automatically removed

## State Restoration

On startup, all bad actors are restored to HAProxy with the configured `block_duration`. This ensures bad actors remain blocked across restarts.

## Cluster Behavior

- Scoring is global across all websites
- Events are synced across nodes via the existing state sync mechanism
- Each node promotes independently when the threshold is reached
- Bad actors are included in the state sync (via the `ips` table with reason "bad-actor")

## Config Hot-Reload

| Change | Behavior |
|:---|:---|
| Threshold change | Applies to new blocks only; existing scores not recalculated |
| Chain weight change | Applies to new blocks; historical events keep original weight |
| Block duration change | Applies to new promotions; existing bad actors keep original duration |
