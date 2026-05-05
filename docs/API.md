# Internal HTTP Server API

When the `--listen` flag is used (e.g., `--listen :8080`), the bot-detector starts an internal web server to provide live metrics and access to the current configuration.

This API is intended for monitoring and administrative purposes.

## Multiple Listeners and Role-Based Routing

The `--listen` flag supports multiple listeners with optional role-based routing. API endpoints can be isolated using `role=api`:

```bash
# Dedicated API listener
--listen :8080,role=api

# API and metrics on separate ports
--listen :8080,role=api --listen :9090,role=metrics
```

See the main [README.md](../README.md) for complete `--listen` flag documentation and routing rules.

## Endpoints

### `/help`

*   **Method:** `GET`
*   **Content-Type:** `text/plain; charset=utf-8`
*   **Description:** Lists all available endpoints in a human-readable table with method, path, content type, and description. Available on all listeners regardless of role configuration.

### `/api/v1/help`

*   **Method:** `GET`
*   **Content-Type:** `application/json`
*   **Description:** Lists API endpoints as JSON. Each entry includes `method`, `path`, `description`, `content_type`, and `role`.
*   **Role:** `api`

### `/stats`

*   **Method:** `GET`
*   **Content-Type:** `text/plain; charset=utf-8`
*   **Description:** Displays a comprehensive plain-text report of the application's real-time metrics. This is the main dashboard for monitoring activity. The report includes:
    *   Timestamp and uptime information
    *   General processing statistics (lines processed, valid hits, errors, processing rate)
    *   Actor statistics (good actors skipped, actors cleaned)
    *   Chain and action statistics
    *   Per-chain metrics (hits, completions, resets) - only active chains shown
    *   Website information for multi-website mode (shown as `[website1, website2]` after chain name)

### `/stats/steps`

*   **Method:** `GET`
*   **Content-Type:** `text/plain; charset=utf-8`
*   **Description:** Provides a plain text report of behavioral chain step executions grouped by website. The report includes:
    *   Timestamp
    *   Total step executions across all websites
    *   Per-website sections showing:
        *   Website name and execution count for that website
        *   Individual step counts with percentages (calculated against overall total)
        *   Steps sorted by execution count in descending order
    *   Global chains shown first, then website-specific chains alphabetically
    *   Step names include website/vhost context (e.g., `step 1/3 of ChainName[website]`)
    *   Useful for debugging chain performance and comparing activity across websites.

### `/stats/websites`

*   **Method:** `GET`
*   **Content-Type:** `text/plain; charset=utf-8`
*   **Description:** Displays multi-website statistics including configured websites, their vhosts, log paths, chain assignments, and a list of unknown vhosts encountered in logs. The report includes:
    *   Timestamp
    *   Website configuration details
    *   Per-website metrics (lines parsed, chain matches, completions, resets)
    *   Unknown vhosts list
    *   This endpoint is particularly useful for:
        *   Verifying multi-website configuration
        *   Identifying misconfigured or missing vhosts
        *   Troubleshooting log entries that are being skipped
*   **Response (Multi-Website Mode):**
    ```
    Generated: 2026-03-10T09:15:00+01:00

    === Multi-Website Statistics ===
    
    Total Websites: 2
    Global Chains: 1
    Website-Specific Chains: 2
    
    === Configured Websites ===
      main:
        VHosts: www.example.com, example.com
        Log Path: /var/log/haproxy/main.log
        Chains: 1
      api:
        VHosts: api.example.com
        Log Path: /var/log/haproxy/api.log
        Chains: 1
    
    --- Unknown VHosts ---
      Total: 1
      VHosts:
        - unknown.example.com
    
      Note: Unknown vhosts are logged once and their entries are skipped.
      To fix: Add the vhost to a website's 'vhosts' list in config.yaml
    ```
*   **Response (Single-Website Mode):**
    ```
    Multi-website mode is not enabled.
    To enable, add a 'websites' section to your config.yaml
    ```

### `/stats/bad-actors`

*   **Method:** `GET`
*   **Content-Type:** `text/plain; charset=utf-8`
*   **Description:** Human-readable bad actor statistics. Shows total count, average score and block count, IPs per reason (sorted by count descending), and promotions per day (sorted chronologically). JSON equivalent: `/api/v1/bad-actors/stats`.
*   **Response:**
    ```
    === Bad Actor Statistics ===
    Total: 22078
    Avg Score: 5.0
    Avg Block Count: 5.0

    === IPs per Reason ===
        2307  Bad-WS-User-Agent@musicbrainz.org
         546  No-Referrer-Subpage-Crawler@musicbrainz.org
         170  Bad-User-Agent-Example-com@musicbrainz.org

    === Promotions per Day ===
        4544  2026-04-08
       11442  2026-04-09
        6092  2026-04-10
    ```

### `/stats/parse-errors`

*   **Method:** `GET`
*   **Content-Type:** `text/plain; charset=utf-8`
*   **Description:** Returns the most recent log lines that failed to parse (newest first, one per line). The buffer size is controlled by `application.max_recent_parse_errors` (default: 50, set to 0 to disable).
*   **Response:**
    ```
    musicbrainz.org 34.58.6.41 - - [15/Apr/2026:11:44:05 +0000] "" 400 0 "-" "-"
    musicbrainz.org 1.2.3.4 - - [15/Apr/2026:11:43:00 +0000] "GET /incomplete HTTP/1.1" 200 100 "-"
    ```

### `/config`

*   **Method:** `GET`
*   **Content-Type:** `application/yaml`
*   **Description:** Returns the raw YAML content of the main configuration file as it was last loaded by the application. This allows you to inspect the exact configuration that is currently active, which is especially useful after a hot-reload.
*   **Responses:**
    *   `200 OK`: Successfully returns the YAML configuration.
    *   `500 Internal Server Error`: If the server fails to retrieve or marshal the configuration content.

### `/config/archive`

*   **Method:** `GET`
*   **Content-Type:** `application/gzip`
*   **Description:** Downloads a `.tar.gz` archive containing the active `config.yaml` and all of its file-based dependencies (e.g., files referenced with `file:`). This provides a complete, portable snapshot of a working configuration, which can be used for backups or for migrating the exact same ruleset to another bot-detector instance. The archive preserves the relative directory structure of the dependencies.
*   **Responses:**
    *   `200 OK`: Successfully begins streaming the gzipped tar archive.
    *   `500 Internal Server Error`: If the server fails to access the configuration or its dependencies to create the archive.


## Bad Actors Endpoints

### `GET /api/v1/bad-actors`

*   **Method:** `GET`
*   **Content-Type:** `application/json`
*   **Description:** Returns all IPs that have been promoted to bad actor status. Each entry includes the IP, promotion timestamp, total score, block count, and a JSON history of the block events that led to promotion.
*   **Role:** `api`

### `GET /api/v1/bad-actors/export`

*   **Method:** `GET`
*   **Content-Type:** `text/plain`
*   **Description:** Returns all bad actor IPs as plain text, one per line. Useful for integration with external firewalls, blocklists, or other security tools.
*   **Role:** `api`

### `GET /api/v1/bad-actors/stats`

*   **Method:** `GET`
*   **Content-Type:** `application/json`
*   **Description:** Returns aggregated statistics about bad actors: total count, average score and block count, number of distinct IPs per reason (chain name), and promotions per day. Useful for identifying overzealous chains or spotting spikes in bad actor promotions.
*   **Role:** `api`
*   **Response Format:**
    ```json
    {
      "total": 42,
      "avg_score": 6.3,
      "avg_block_count": 8,
      "by_reason": {
        "aggressive-scraper@example.com": 15,
        "rate-limit-api@example.com": 10,
        "SQL-Injection": 17
      },
      "by_day": {
        "2026-04-08": 5,
        "2026-04-09": 12,
        "2026-04-10": 25
      }
    }
    ```
*   **Notes:**
    *   `by_reason` counts distinct IPs per reason — an IP with multiple block events for the same reason is counted once.
    *   An IP can appear under multiple reasons if it triggered different chains.
    *   `by_day` is keyed by the promotion date (not block date).

### `DELETE /api/v1/bad-actors?reason=<reason>[&unblock]` (Cluster-Aware)

*   **Method:** `DELETE`
*   **Content-Type:** `application/json`
*   **Description:** Removes all bad actors whose block history contains the given reason substring. This is useful when a chain was overzealous and has been modified or removed — IPs that were promoted to bad actor status because of that chain can be cleared in bulk. The match is performed against the `"r"` field in each bad actor's history JSON. Both the bad actor entry and its accumulated score are removed.
*   **Role:** `api`
*   **Parameters:**
    *   `reason` (query, required) - Substring to match against chain reasons in the bad actor history. Typically a chain name (e.g., `rate-limit-api`) or a vhost-qualified reason (e.g., `rate-limit-api@example.com`).
    *   `unblock` (query, optional, no value) - If present, also unblocks the removed IPs from HAProxy (sets `gpc0=0`), clears persistence state, and removes from the activity store.
*   **Cluster Behavior:**
    *   **Follower nodes:** Forward the request to the leader and return the leader's response.
    *   **Leader node:** Remove locally, broadcast removal to all followers, then process `&unblock` (which also broadcasts to followers).
    *   **Standalone node:** Remove locally only.
*   **Response Format:**
    ```json
    {
      "reason": "rate-limit-api",
      "removed": 3,
      "ips": ["1.2.3.4", "5.6.7.8", "9.10.11.12"]
    }
    ```
*   **Response Format (with `&unblock`):**
    ```json
    {
      "reason": "rate-limit-api",
      "removed": 3,
      "ips": ["1.2.3.4", "5.6.7.8", "9.10.11.12"],
      "unblocked": 3
    }
    ```
*   **Response Format (with `&unblock`, partial failure):**
    ```json
    {
      "reason": "rate-limit-api",
      "removed": 3,
      "ips": ["1.2.3.4", "5.6.7.8", "9.10.11.12"],
      "unblocked": 2,
      "unblock_errors": ["9.10.11.12"]
    }
    ```
*   **Error Response (400 Bad Request):**
    ```
    reason query parameter is required
    ```
*   **Notes:**
    *   The match is a **substring match** — `reason=rate-limit` will match `rate-limit-api`, `rate-limit-static`, etc. Use the full chain name for precision.
    *   In multi-website mode, reasons include the vhost or website suffix (e.g., `chainName@vhost` or `chainName[website]`). You can match on just the chain name to clear across all websites, or include the suffix to target a specific website.
    *   Without `&unblock`, this only removes the bad actor record and score from the database. The IPs may still be actively blocked in HAProxy until their block duration expires.
    *   With `&unblock`, each removed IP is also unblocked from HAProxy (sets `gpc0=0`), cleared from persistence, and removed from the activity store.
*   **Usage Examples:**
    ```bash
    # Remove all bad actors promoted by the "aggressive-scraper" chain
    curl -X DELETE 'http://localhost:8080/api/v1/bad-actors?reason=aggressive-scraper'

    # Remove and unblock from HAProxy in one call
    curl -X DELETE 'http://localhost:8080/api/v1/bad-actors?reason=aggressive-scraper&unblock'

    # Remove bad actors from a specific website
    curl -X DELETE 'http://localhost:8080/api/v1/bad-actors?reason=aggressive-scraper@example.com'

    # Preview which bad actors would match (check history first)
    curl -s http://localhost:8080/api/v1/bad-actors | jq '.[].history' 
    ```
*   **Responses:**
    *   `200 OK`: Successfully processed the request (even if no bad actors matched).
    *   `400 Bad Request`: Missing `reason` query parameter.
    *   `500 Internal Server Error`: Database error during removal.
    *   `502 Bad Gateway`: (Follower only) Failed to forward request to leader.

See [BAD_ACTORS.md](BAD_ACTORS.md) for full documentation of the bad actors feature, including configuration, scoring, and removal.

## Cluster Endpoints

These endpoints are available when cluster mode is enabled. They provide cluster status, metrics collection, and aggregation capabilities.

Each cluster endpoint has two variants:
- `/cluster/*` — Human-readable plain text output
- `/api/v1/cluster/*` — JSON output for programmatic access

The internal cluster communication uses the `/api/v1/` JSON endpoints.

### `/cluster/status` and `/api/v1/cluster/status`

*   **Method:** `GET`
*   **Content-Type:** `text/plain` (`/cluster/status`) or `application/json` (`/api/v1/cluster/status`)
*   **Description:** Returns the current node's cluster identity and role information.
*   **Plain Text Response:**
    ```
    role: leader
    name: node-1
    address: localhost:8080
    ```
*   **JSON Response:**
    ```json
    {
      "role": "leader",
      "name": "node-1",
      "address": "localhost:8080"
    }
    ```
*   **Responses:**
    *   `200 OK`: Successfully returns the node status.
    *   `500 Internal Server Error`: If the server fails to retrieve node status information.

### `/cluster/metrics` and `/api/v1/cluster/metrics`

*   **Method:** `GET`
*   **Content-Type:** `text/plain` (`/cluster/metrics`) or `application/json` (`/api/v1/cluster/metrics`)
*   **Description:** Returns this node's current metrics. The JSON variant is used internally by leader nodes to collect metrics from followers.
*   **Plain Text Response:**
    ```
    timestamp: 2025-11-18T20:30:00Z
    lines_processed: 1000
    entries_checked: 42
    parse_errors: 1
    lines_per_second: 95.2
    actions_block: 15
    actions_log: 27
    chains_completed: 42
    chains_reset: 1
    good_actors_skipped: 10
    actors_cleaned: 5
    ```
*   **JSON Response:**
    ```json
    {
      "timestamp": "2025-11-18T20:30:00Z",
      "processing_stats": {
        "lines_processed": 1000,
        "entries_checked": 42,
        "parse_errors": 1,
        "reordered_lines": 2,
        "time_elapsed_seconds": 10.5,
        "lines_per_second": 95.24
      },
      "actor_stats": {
        "good_actors_skipped": 10,
        "actors_cleaned": 5
      },
      "chain_stats": {
        "actions_block": 15,
        "actions_log": 27,
        "total_hits": 695,
        "completed": 42,
        "resets": 1
      },
      "good_actor_hits": {
        "known_bot": 4,
        "monitoring_agent": 2,
        "our_network": 4
      },
      "skips_by_reason": {
        "blocked:SimpleBlockChain": 2,
        "good_actor:known_bot": 4
      },
      "match_key_hits": {
        "ip": 652,
        "ip_ua": 41,
        "ipv6": 2
      },
      "block_durations": {
        "1h": 1,
        "30m": 14
      },
      "per_chain_metrics": {
        "SimpleBlockChain": {
          "hits": 2,
          "completed": 1,
          "resets": 0
        },
        "SimpleLogChain": {
          "hits": 4,
          "completed": 2,
          "resets": 0
        }
      },
      "website_metrics": {
        "main_site": {
          "lines_parsed": 450,
          "chains_matched": 12,
          "chains_reset": 1,
          "chains_completed": 8
        },
        "api_site": {
          "lines_parsed": 550,
          "chains_matched": 30,
          "chains_reset": 0,
          "chains_completed": 34
        }
      }
    }
    ```
*   **Note:** The `website_metrics` field is only present when multi-website mode is enabled.
*   **Responses:**
    *   `200 OK`: Successfully returns the metrics snapshot.
    *   `500 Internal Server Error`: If the server fails to generate the metrics snapshot.

### `/cluster/metrics/aggregate` and `/api/v1/cluster/metrics/aggregate`

*   **Method:** `GET`
*   **Content-Type:** `text/plain` (`/cluster/metrics/aggregate`, indented JSON) or `application/json` (`/api/v1/cluster/metrics/aggregate`)
*   **Description:** Returns cluster-wide aggregated metrics from all nodes (leader only). The plain text variant returns indented JSON for readability.
*   **Response Format:**
    ```json
    {
      "timestamp": "2025-11-18T20:30:00Z",
      "total_nodes": 3,
      "healthy_nodes": 2,
      "stale_nodes": 0,
      "error_nodes": 1,
      "aggregated": {
        "timestamp": "2025-11-18T20:30:00Z",
        "processing_stats": {
          "lines_processed": 3000,
          "entries_checked": 126,
          "parse_errors": 3,
          "reordered_lines": 6,
          "time_elapsed_seconds": 31.5,
          "lines_per_second": 95.24
        },
        "actor_stats": {
          "good_actors_skipped": 30,
          "actors_cleaned": 15
        },
        "chain_stats": {
          "actions_block": 45,
          "actions_log": 81,
          "total_hits": 2085,
          "completed": 126,
          "resets": 3
        },
        "good_actor_hits": {
          "known_bot": 12,
          "monitoring_agent": 6,
          "our_network": 12
        },
        "per_chain_metrics": {
          "SimpleBlockChain": {
            "hits": 6,
            "completed": 3,
            "resets": 0
          }
        },
        "website_metrics": {
          "main_site": {
            "lines_parsed": 1350,
            "chains_matched": 36,
            "chains_reset": 3,
            "chains_completed": 24
          },
          "api_site": {
            "lines_parsed": 1650,
            "chains_matched": 90,
            "chains_reset": 0,
            "chains_completed": 102
          }
        }
      },
      "nodes": [
        {
          "node_name": "follower-1",
          "address": "localhost:9090",
          "status": "healthy",
          "last_collected": "2025-11-18T20:29:55Z",
          "consecutive_errors": 0,
          "metrics": {
            "timestamp": "2025-11-18T20:29:55Z",
            "processing_stats": {
              "lines_processed": 1000,
              "entries_checked": 42
            }
          }
        },
        {
          "node_name": "follower-2",
          "address": "localhost:9091",
          "status": "healthy",
          "last_collected": "2025-11-18T20:29:54Z",
          "consecutive_errors": 0,
          "metrics": {
            "timestamp": "2025-11-18T20:29:54Z",
            "processing_stats": {
              "lines_processed": 1000,
              "entries_checked": 42
            }
          }
        },
        {
          "node_name": "follower-3",
          "address": "localhost:9092",
          "status": "error",
          "last_collected": "2025-11-18T20:25:10Z",
          "consecutive_errors": 5,
          "last_error": "HTTP request failed: connection refused"
        }
      ]
    }
    ```
*   **Node Health Status:**
    *   `"healthy"`: Node is responding normally and metrics are fresh (within 3x the poll interval)
    *   `"stale"`: Node metrics are outdated (last collection > 3x poll interval)
    *   `"error"`: Node has consecutive errors or no snapshot available
*   **Responses:**
    *   `200 OK`: Successfully returns aggregated cluster metrics.
    *   `404 Not Found`: This endpoint is only available on leader nodes. Returned when querying a follower node.
    *   `500 Internal Server Error`: If the server fails to aggregate metrics.


## IP Lookup Endpoints

These endpoints allow you to query the block/unblock status of specific IP addresses. They provide visibility into which IPs are currently blocked, why they were blocked, and when blocks will expire.

### `/ip/{ip}` (Cluster-Aware)

*   **Method:** `GET`
*   **Content-Type:** `text/plain; charset=utf-8`
*   **Description:** Returns the block/unblock status of an IP address. This endpoint is cluster-aware and automatically provides the appropriate view based on deployment:
    *   **Follower nodes:** Forward the request to the leader and return cluster-wide aggregated status
    *   **Leader nodes:** Query all nodes and return cluster-wide aggregated status
    *   **Standalone nodes:** Return local node status only
    
    The IP address is automatically canonicalized (e.g., `2001:0db8::1` becomes `2001:db8::1`).
*   **Parameters:**
    *   `ip` - IPv4 or IPv6 address (will be canonicalized)
*   **Response Format (Cluster - Blocked):**
    ```
    cluster_status: blocked
    nodes:
      - name: node-1
        status: blocked
        actors: 2
        chains:
          - SimpleBlockChain (until: 2025-11-22T02:00:00Z)
        persistence: blocked
        persistence_expires: 2025-11-22T02:00:00Z
      - name: node-2
        status: unknown
    backend_tables:
      - thirty_min_blocks_v4 on /var/run/haproxy/admin.sock (status: blocked, duration: 30m, expires in: 15m, added: 2025-11-28 10:30:00)
    ```
*   **Response Format (Standalone - Blocked):**
    ```
    node: standalone
    status: blocked
    actors: 2
    chains:
      - SimpleBlockChain (until: 2025-11-22T02:00:00Z)
    persistence: blocked
    persistence_expires: 2025-11-22T02:00:00Z
    backend_tables:
      - thirty_min_blocks_v4 on /var/run/haproxy/admin.sock (status: blocked, duration: 30m, expires in: 15m)
    ```
*   **Response Format (Standalone - Bad Actor):**
    ```
    node: standalone
    status: blocked
    persistence: blocked
    persistence_reason: bad-actor
    bad_actor: yes
    bad_actor_promoted_at: 2026-03-12T16:00:00Z
    bad_actor_score: 5.5
    bad_actor_block_count: 7
    ```
*   **Response Format (Standalone - IP with Score):**
    ```
    node: standalone
    status: blocked
    persistence: blocked
    persistence_reason: SQL-Injection
    score: 2.3 / 5.0
    score_block_count: 4
    ```
*   **Response Format (Unknown IP):**
    ```
    cluster_status: unknown
    nodes:
      - name: node-1
        status: unknown
      - name: node-2
        status: unknown
    ```
*   **Cluster Status Values:**
    *   `"blocked"`: IP is blocked on all nodes that have information about it
    *   `"unblocked"`: IP is not blocked on any node
    *   `"unknown"`: IP is not known to any node
    *   `"mixed"`: IP has different statuses across nodes
*   **Notes:**
    *   Followers automatically get cluster-wide view by forwarding to leader
    *   Leader queries all nodes concurrently (5-second timeout per node)
    *   Provides complete picture of IP status across entire deployment
    *   Shows persistence state from each node
    *   The `actors` field indicates how many IP+UserAgent combinations exist for this IP
    *   Multiple chains can block the same IP if different behavioral patterns are detected
*   **Responses:**
    *   `200 OK`: Successfully returns IP status.
    *   `400 Bad Request`: Invalid IP address format.
    *   `502 Bad Gateway`: (Follower only) Failed to contact leader.

### `/api/v1/ip/{ip}` (Cluster-Aware)

*   **Method:** `GET`
*   **Content-Type:** `application/json`
*   **Description:** Returns the block/unblock status of an IP address in JSON format. This endpoint is cluster-aware and designed for programmatic access:
    *   **Follower nodes:** Forward the request to the leader and return cluster-wide aggregated status
    *   **Leader nodes:** Query all nodes and return cluster-wide aggregated status
    *   **Standalone nodes:** Return local node status only
*   **Parameters:**
    *   `ip` - IPv4 or IPv6 address (will be canonicalized)
*   **Response Format (Cluster - Blocked):**
    ```json
    {
      "cluster_status": "blocked",
      "nodes": [
        {
          "name": "node-1",
          "status": "blocked",
          "actors": 2,
          "chains": {
            "SimpleBlockChain": "2025-11-22T02:00:00Z"
          },
          "persistence": "blocked",
          "persistence_expires": "2025-11-22T02:00:00Z"
        },
        {
          "name": "node-2",
          "status": "unknown"
        }
      ]
    }
    ```
*   **Response Format (Standalone - Blocked IP):**
    ```json
    {
      "node": "standalone",
      "status": "blocked",
      "actors": 2,
      "chains": {
        "SimpleBlockChain": "2025-11-22T02:00:00Z",
        "API-Abuse-Chain": "2025-11-22T01:30:00Z"
      },
      "earliest_block": "2025-11-22T01:00:00Z",
      "latest_expiry": "2025-11-22T02:00:00Z",
      "persistence": "blocked",
      "persistence_expires": "2025-11-22T02:00:00Z"
    }
    ```
*   **Response Format (Standalone - Bad Actor):**
    ```json
    {
      "node": "standalone",
      "status": "blocked",
      "persistence": "blocked",
      "persistence_reason": "bad-actor",
      "bad_actor": {
        "promoted_at": "2026-03-12T16:00:00Z",
        "total_score": 5.5,
        "block_count": 7,
        "history": "[{\"ts\":\"2026-03-12T16:00:00Z\",\"r\":\"SQL-Injection\"}]"
      }
    }
    ```
*   **Response Format (Standalone - IP with Score, not yet bad actor):**
    ```json
    {
      "node": "standalone",
      "status": "blocked",
      "persistence": "blocked",
      "persistence_reason": "SQL-Injection",
      "score": {
        "current_score": 2.3,
        "block_count": 4,
        "threshold": 5.0
      }
    }
    ```
*   **Response Format (Standalone - Unblocked IP):**
    ```json
    {
      "node": "standalone",
      "status": "unblocked",
      "last_seen": "2025-11-22T01:00:00Z",
      "last_unblock": "2025-11-22T00:30:00Z",
      "unblock_reason": "good-actor:monitoring_agent"
    }
    ```
*   **Response Format (Unknown IP):**
    ```json
    {
      "cluster_status": "unknown",
      "nodes": [
        {
          "name": "node-1",
          "status": "unknown"
        }
      ]
    }
    ```
*   **Notes:**
    *   All timestamps are in RFC3339 format (ISO 8601)
    *   IPv6 addresses are canonicalized (e.g., `2001:0db8::1` → `2001:db8::1`)
    *   Followers automatically get cluster-wide view by forwarding to leader
    *   Leader queries all nodes concurrently (5-second timeout per node)
    *   Standalone nodes return local `IPStatusResponse` format
    *   Cluster nodes return `ClusterIPAggregateResponse` format
*   **Responses:**
    *   `200 OK`: Successfully returns IP status.
    *   `400 Bad Request`: Invalid IP address format.
    *   `502 Bad Gateway`: (Follower only) Failed to contact leader.

    }
    ```
*   **Error Response (400 Bad Request):**
    ```json
    {
      "error": "Invalid IP address"
    }
    ```
*   **Notes:**
    *   All timestamps are in RFC3339 format (ISO 8601)
    *   IPv6 addresses are canonicalized (e.g., `2001:0db8::1` → `2001:db8::1`)
    *   The `chains` object maps chain names to their expiry times
    *   The `bad_actor` object is present only when the IP has been promoted to bad actor status
    *   The `score` object is present only when the IP has a score but is not yet a bad actor
    *   The `cluster_hint` field (on followers) provides the URL to query for cluster-wide status
*   **Responses:**
    *   `200 OK`: Successfully returns IP status.
    *   `400 Bad Request`: Invalid IP address format.

### `/ip/{ip}/clear` (DELETE)

*   **Method:** `DELETE`
*   **Content-Type:** `text/plain; charset=utf-8`
*   **Description:** Clears an IP address from all state: HAProxy stick tables, activity store, persistence, bad actor status, and score. This is a cluster-aware operation - if called on a follower, the request is forwarded to the leader, which then broadcasts the clear command to all nodes. Each node independently clears the IP from its local HAProxy tables and persistence state.
*   **Parameters:**
    *   `ip` - IPv4 or IPv6 address (will be canonicalized)
*   **Response Format (Success - IP found):**
    ```
    IP 192.168.1.100 found and cleared from:
      - thirty_min_blocks_v4 on /var/run/haproxy/admin.sock (status: blocked, duration: 30m, expires in: 15m, added: 2025-11-28 10:30:00)
      - one_hour_blocks_v4 on /var/run/haproxy/admin.sock (status: blocked, duration: 1h, expires in: 45m, added: 2025-11-28 10:00:00)
    ```
*   **Response Format (Success - IP not found):**
    ```
    IP 192.168.1.100 not found in any tables
    ```
*   **Error Response (400 Bad Request):**
    ```
    Invalid IP address
    ```
*   **Error Response (502 Bad Gateway - Follower only):**
    ```
    Failed to contact leader: connection refused
    ```
*   **Error Response (503 Service Unavailable):**
    ```
    Blocker not available
    ```
*   **Error Response (500 Internal Server Error):**
    ```
    Failed to clear IP: <error details>
    ```
*   **Cluster Behavior:**
    *   **Follower nodes:** Forward the request to the leader and return the leader's response
    *   **Leader node:** Clear locally, then broadcast to all followers asynchronously
    *   **Standalone node:** Clear locally only
*   **What gets cleared:**
    *   All HAProxy stick table entries for the IP (across all duration tables) - **removed completely**
    *   IP from persistence state (`IPStates` map)
    *   Unblock event written to journal with reason "manual_clear"
*   **Performance Note:**
    *   Removing entries from HAProxy tables is **slow** for large tables
    *   For quick unblocking, use `/ip/{ip}/unblock` instead (sets `gpc0=0`, much faster)
*   **Notes:**
    *   The IP address is canonicalized before processing (e.g., `2001:0db8::1` → `2001:db8::1`)
    *   Clears from all configured HAProxy instances and all duration tables
    *   Works for both blocked and unblocked IPs (clears all state regardless of status)
    *   The operation is logged on each node that processes it
    *   Broadcast to followers is asynchronous (fire-and-forget) - leader doesn't wait for follower responses
    *   If a follower is unreachable during broadcast, it's logged but doesn't fail the request
    *   Use this when you want to completely remove an IP from the system (e.g., false positive, testing)
*   **Responses:**
    *   `200 OK`: Successfully cleared the IP (or IP not found).
    *   `400 Bad Request`: Invalid IP address format.
    *   `500 Internal Server Error`: Failed to clear the IP from HAProxy or persistence.
    *   `502 Bad Gateway`: (Follower only) Failed to forward request to leader.
    *   `503 Service Unavailable`: Blocker is not available (e.g., dry-run mode).

### `/ip/{ip}/unblock` (GET or POST)

*   **Method:** `GET` or `POST`
*   **Content-Type:** `text/plain; charset=utf-8` (POST) or cluster status format (GET)
*   **Description:** Fast unblock operation that sets `gpc0=0` in HAProxy stick tables without removing the entry. The entry remains in the table and expires naturally. This is **much faster** than `/clear` for large tables. Cluster-aware - forwards to leader if called on follower, then broadcasts to all nodes.
*   **Parameters:**
    *   `ip` - IPv4 or IPv6 address (will be canonicalized)
*   **Response Format (POST - Simple confirmation):**
    ```
    IP 192.168.1.100 unblocked (gpc0 set to 0, entry will expire naturally)
    ```
*   **Response Format (GET - Unblock + Status):**
    ```
    cluster_status: unblocked
    nodes:
      - name: node1
        status: unblocked
        last_unblock: 2026-03-10T15:09:00Z
        reason: API unblock
      - name: node2
        status: unblocked
        last_unblock: 2026-03-10T15:09:00Z
        reason: API unblock
    ```
*   **Cluster Behavior:**
    *   **Follower nodes:** Forward the request to the leader (preserves GET/POST method)
    *   **Leader node:** Unblock locally, then broadcast to all followers asynchronously
    *   **Standalone node:** Unblock locally only
*   **What gets updated:**
    *   HAProxy stick tables: `gpc0` set to 0 (entry remains, expires naturally)
    *   Persistence state: Unblock event written to journal with reason "API unblock"
    *   Activity store: IP removed from in-memory chain progress
    *   Bad actor status: Removed from `bad_actors` and `ip_scores` tables (same as `/clear`)
*   **Performance:**
    *   **Fast:** Only updates `gpc0` value, doesn't remove entry from table
    *   Recommended for day-to-day unblocking operations
    *   Entry will be removed by HAProxy when it naturally expires
*   **Usage Examples:**
    ```bash
    # Quick unblock with status confirmation (GET)
    curl http://localhost:8092/ip/192.168.1.100/unblock
    
    # Simple unblock (POST)
    curl -X POST http://localhost:8092/ip/192.168.1.100/unblock
    ```
*   **Comparison with `/clear`:**
    *   `/unblock`: Sets `gpc0=0`, entry expires naturally (fast)
    *   `/clear`: Removes entry completely from table (slow)
*   **Responses:**
    *   `200 OK`: Successfully unblocked the IP.
    *   `400 Bad Request`: Invalid IP address format.
    *   `500 Internal Server Error`: Failed to unblock the IP.
    *   `502 Bad Gateway`: (Follower only) Failed to forward request to leader.
    *   `503 Service Unavailable`: Blocker is not available (e.g., dry-run mode).

### `POST /api/v1/ip/{ip}/clear` (Cluster-Aware)

*   **Method:** `POST`
*   **Content-Type:** `application/json`
*   **Description:** JSON equivalent of `DELETE /ip/{ip}/clear`. Clears an IP from all HAProxy stick tables, persistence, and activity store.
*   **Role:** `api`
*   **Response Format:**
    ```json
    {
      "ip": "192.168.1.100",
      "cleared": 2,
      "tables": [
        {
          "table": "thirty_min_blocks_v4",
          "backend": "/var/run/haproxy/admin.sock",
          "status": "blocked",
          "duration": "30m",
          "expires_in": "15m"
        }
      ]
    }
    ```
*   **Responses:**
    *   `200 OK`: Successfully cleared the IP.
    *   `400 Bad Request`: Invalid IP address format.
    *   `500 Internal Server Error`: Failed to clear the IP.
    *   `502 Bad Gateway`: (Follower only) Failed to forward request to leader.
    *   `503 Service Unavailable`: Blocker is not available.

### `POST /api/v1/ip/{ip}/unblock` (Cluster-Aware)

*   **Method:** `POST`
*   **Content-Type:** `application/json`
*   **Description:** JSON equivalent of `/ip/{ip}/unblock`. Sets `gpc0=0` in HAProxy stick tables without removing the entry.
*   **Role:** `api`
*   **Response Format:**
    ```json
    {
      "ip": "192.168.1.100",
      "status": "unblocked"
    }
    ```
*   **Responses:**
    *   `200 OK`: Successfully unblocked the IP.
    *   `400 Bad Request`: Invalid IP address format.
    *   `500 Internal Server Error`: Failed to unblock the IP.
    *   `502 Bad Gateway`: (Follower only) Failed to forward request to leader.
    *   `503 Service Unavailable`: Blocker is not available.

### `POST /api/v1/blocks/unblock?reason=<reason>` (Cluster-Aware)

*   **Method:** `POST`
*   **Content-Type:** `application/json`
*   **Description:** Unblocks all IPs currently blocked with a reason matching the given substring. Queries the persistence database for blocked IPs whose reason contains the substring, then unblocks each one from HAProxy. Useful for bulk-unblocking after a chain is modified or removed.
*   **Role:** `api`
*   **Parameters:**
    *   `reason` (query, required) - Substring to match against block reasons. Typically a chain name (e.g., `Bad-User-Agent@musicbrainz.org`).
*   **Cluster Behavior:**
    *   **Follower nodes:** Forward the request to the leader.
    *   **Leader node:** Query local persistence, unblock each IP (broadcasts to followers).
    *   **Standalone node:** Query and unblock locally.
*   **Response Format:**
    ```json
    {
      "reason": "Bad-User-Agent@musicbrainz.org",
      "matched": 150,
      "unblocked": 148
    }
    ```
*   **Response Format (partial failure):**
    ```json
    {
      "reason": "Bad-User-Agent@musicbrainz.org",
      "matched": 150,
      "unblocked": 148,
      "errors": ["1.2.3.4", "5.6.7.8"]
    }
    ```
*   **Notes:**
    *   Only affects IPs currently in `blocked` state with a non-expired block. Already-expired blocks are not included.
    *   The reason match is a **substring match** — use the full chain name for precision.
    *   For bad actors (permanent blocks), use `DELETE /api/v1/bad-actors?reason=...&unblock` instead.
    *   With many matched IPs, the unblock commands go through the rate-limited queue. Ensure `commands_per_second` is high enough.
*   **Responses:**
    *   `200 OK`: Successfully processed (even if no IPs matched).
    *   `400 Bad Request`: Missing `reason` query parameter.
    *   `500 Internal Server Error`: Database error.
    *   `502 Bad Gateway`: (Follower only) Failed to forward request to leader.


### `/api/v1/cluster/internal/ip/{ip}` (Internal Use)

*   **Method:** `GET`
*   **Content-Type:** `application/json`
*   **Description:** Internal endpoint used by the leader to query follower nodes for IP status. This endpoint returns the same information as `/api/v1/ip/{ip}` but without the `node` and `cluster_hint` fields, as these are added by the leader during aggregation. This endpoint is not intended for direct user access.
*   **Parameters:**
    *   `ip` - IPv4 or IPv6 address (will be canonicalized)
*   **Response Format:** Same as `/api/v1/ip/{ip}` but without `node` and `cluster_hint` fields.
*   **Responses:**
    *   `200 OK`: Successfully returns IP status.
    *   `400 Bad Request`: Invalid IP address format.


## Challenge API

### `GET /api/v1/challenge/{website}/{ip}`

Check if an IP is currently challenged on a website.

**Response:**
```json
{"ip": "1.2.3.4", "website": "mb_prod", "challenged": true, "reason": "Distributed-Entity-SubPage-Scraper"}
```

### `POST /api/v1/challenge/{website}/{ip}`

Manually challenge an IP on a website.

**Query parameters:**
- `duration` (optional): Challenge TTL, e.g. `12h`. Default: `24h`.
- `reason` (optional): Reason string. Default: `manual`.

**Response:**
```json
{"ip": "1.2.3.4", "website": "mb_prod", "duration": "24h0m0s", "reason": "manual", "status": "challenged"}
```

### `DELETE /api/v1/challenge/{website}/{ip}`

Remove a challenge for an IP on a website.

**Response:**
```json
{"ip": "1.2.3.4", "website": "mb_prod", "status": "unchallenged"}
```


## Usage Examples

### Query IP status (cluster-aware)
```bash
# Works on any node - automatically provides cluster-wide view in cluster mode
curl http://localhost:8080/ip/192.168.1.100
```

### Query IP status JSON (cluster-aware)
```bash
# Returns cluster aggregation on leader/follower, local view on standalone
curl http://localhost:8080/api/v1/ip/192.168.1.100
```

### Clear an IP from all state (cluster-aware)
```bash
curl -X DELETE http://localhost:8080/ip/192.168.1.100/clear
```

### Clear IPv6 address
```bash
curl -X DELETE http://localhost:8080/ip/2001:db8::1/clear
```

### Query IPv6 address (automatically canonicalized)
```bash
curl http://localhost:8080/ip/2001:0db8::1
# Queries for canonical form: 2001:db8::1
```

### Check if IP is blocked (script example)
```bash
#!/bin/bash
IP="192.168.1.100"
STATUS=$(curl -s "http://localhost:8080/api/v1/ip/$IP" | jq -r '.status')

if [ "$STATUS" = "blocked" ]; then
    echo "IP $IP is currently blocked"
    exit 1
else
    echo "IP $IP is not blocked"
    exit 0
fi
```

### Clear IP if blocked (script example)
```bash
#!/bin/bash
IP="192.168.1.100"
STATUS=$(curl -s "http://localhost:8080/api/v1/ip/$IP" | jq -r '.status')

if [ "$STATUS" = "blocked" ]; then
    echo "IP $IP is blocked, clearing..."
    curl -X DELETE "http://localhost:8080/ip/$IP/clear"
else
    echo "IP $IP is not blocked"
fi
```

### Check persistence state (script example)
```bash
#!/bin/bash
IP="192.168.1.100"
RESPONSE=$(curl -s "http://localhost:8080/api/v1/ip/$IP")
PERSISTENCE=$(echo "$RESPONSE" | jq -r '.persistence // "none"')

if [ "$PERSISTENCE" != "none" ]; then
    echo "IP $IP is in persistence state: $PERSISTENCE"
    EXPIRES=$(echo "$RESPONSE" | jq -r '.persistence_expires // "unknown"')
    echo "Expires: $EXPIRES"
else
    echo "IP $IP is not in persistence"
fi
```

