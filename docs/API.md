# Internal HTTP Server API

When the `--http-server` flag is used (e.g., `--http-server "127.0.0.1:8080"`), the bot-detector starts an internal web server to provide live metrics and access to the current configuration.

This API is intended for monitoring and administrative purposes.

## Endpoints

### **`/` or `/stats`**

*   **Method:** `GET`
*   **Content-Type:** `text/html; charset=utf-8`
*   **Description:** Displays a comprehensive HTML report of the application's real-time metrics. This is the main dashboard for monitoring activity. The report includes:
    *   General processing statistics (lines processed, valid hits, errors).
    *   Actor statistics (good actors skipped, actors cleaned).
    *   Chain and action statistics.
    *   Per-chain metrics (hits, completions, resets).

### **`/stats/steps`**

*   **Method:** `GET`
*   **Content-Type:** `text/plain; charset=utf-8`
*   **Description:** Provides a plain text list of all behavioral chain steps and the number of times each has been executed. This is useful for debugging chain performance and identifying which rules are matching most frequently. The list is sorted by execution count in descending order.

### **`/config`**

*   **Method:** `GET`
*   **Content-Type:** `text/yaml; charset=utf-8`
*   **Description:** Returns the raw YAML content of the main configuration file as it was last loaded by the application. This allows you to inspect the exact configuration that is currently active, which is especially useful after a hot-reload.
*   **Responses:**
    *   `200 OK`: Successfully returns the YAML configuration.
    *   `500 Internal Server Error`: If the server fails to retrieve or marshal the configuration content.

### **`/config/archive`**

*   **Method:** `GET`
*   **Content-Type:** `application/gzip`
*   **Description:** Downloads a `.tar.gz` archive containing the active `config.yaml` and all of its file-based dependencies (e.g., files referenced with `file:`). This provides a complete, portable snapshot of a working configuration, which can be used for backups or for migrating the exact same ruleset to another bot-detector instance. The archive preserves the relative directory structure of the dependencies.
*   **Responses:**
    *   `200 OK`: Successfully begins streaming the gzipped tar archive.
    *   `500 Internal Server Error`: If the server fails to access the configuration or its dependencies to create the archive.

---

## Cluster Endpoints

These endpoints are available when cluster mode is enabled. They provide cluster status, metrics collection, and aggregation capabilities.

### **`/cluster/status`**

*   **Method:** `GET`
*   **Content-Type:** `application/json`
*   **Description:** Returns the current node's cluster identity and role information. This endpoint is useful for determining which node you're connected to and its role in the cluster.
*   **Response Format (Leader):**
    ```json
    {
      "role": "leader",
      "name": "node-1",
      "address": "localhost:8080"
    }
    ```
*   **Response Format (Follower):**
    ```json
    {
      "role": "follower",
      "name": "node-2",
      "address": "localhost:9090",
      "leader": "node-1:8080"
    }
    ```
*   **Responses:**
    *   `200 OK`: Successfully returns the node status.
    *   `500 Internal Server Error`: If the server fails to retrieve node status information.

### **`/cluster/metrics`**

*   **Method:** `GET`
*   **Content-Type:** `application/json`
*   **Description:** Returns this node's current metrics snapshot in JSON format. This endpoint is used by leader nodes to collect metrics from follower nodes, but can also be queried directly for monitoring individual nodes. The metrics include processing statistics, actor statistics, chain execution statistics, and various performance counters.
*   **Response Format:**
    ```json
    {
      "timestamp": "2025-11-18T20:30:00Z",
      "processing_stats": {
        "lines_processed": 1000,
        "valid_hits": 42,
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
      }
    }
    ```
*   **Responses:**
    *   `200 OK`: Successfully returns the metrics snapshot.
    *   `500 Internal Server Error`: If the server fails to generate the metrics snapshot.

### **`/cluster/metrics/aggregate`**

*   **Method:** `GET`
*   **Content-Type:** `application/json`
*   **Description:** Returns cluster-wide aggregated metrics from all nodes (leader only). This endpoint provides a comprehensive view of the entire cluster's performance, including per-node health status and cluster-wide metric summation. Only available on leader nodes; follower nodes will return a 404 error.
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
          "valid_hits": 126,
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
              "valid_hits": 42
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
              "valid_hits": 42
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

---

## IP Lookup Endpoints

These endpoints allow you to query the block/unblock status of specific IP addresses. They provide visibility into which IPs are currently blocked, why they were blocked, and when blocks will expire.

### **`/ip/{ip}`**

*   **Method:** `GET`
*   **Content-Type:** `text/plain; charset=utf-8`
*   **Description:** Returns the block/unblock status of an IP address on the local node in human-readable plain text format. The IP address is automatically canonicalized (e.g., `2001:0db8::1` becomes `2001:db8::1`). This endpoint is useful for quick manual queries via curl or browser.
*   **Parameters:**
    *   `ip` - IPv4 or IPv6 address (will be canonicalized)
*   **Response Format (Blocked IP):**
    ```
    node: follower-2
    status: blocked
    actors: 2
    chains:
      - SimpleBlockChain (until: 2025-11-22T02:00:00Z)
      - API-Abuse-Chain (until: 2025-11-22T01:30:00Z)
    earliest_block: 2025-11-22T01:00:00Z
    latest_expiry: 2025-11-22T02:00:00Z
    note: For cluster-wide view, query leader at http://leader:8080/cluster/ip/192.168.1.1
    ```
*   **Response Format (Unblocked IP):**
    ```
    node: follower-2
    status: unblocked
    last_seen: 2025-11-22T01:00:00Z
    last_unblock: 2025-11-22T00:30:00Z
    reason: good-actor:monitoring_agent
    ```
*   **Response Format (Unknown IP):**
    ```
    node: follower-2
    status: unknown
    ```
*   **Notes:**
    *   The `actors` field indicates how many IP+UserAgent combinations exist for this IP
    *   Multiple chains can block the same IP if different behavioral patterns are detected
    *   The `earliest_block` time is estimated (actual block time - 1 hour) since exact block time is not stored
    *   If queried on a follower node, a hint is provided to query the leader for cluster-wide view
*   **Responses:**
    *   `200 OK`: Successfully returns IP status.
    *   `400 Bad Request`: Invalid IP address format.

### **`/api/v1/ip/{ip}`**

*   **Method:** `GET`
*   **Content-Type:** `application/json`
*   **Description:** Returns the block/unblock status of an IP address on the local node in JSON format. This endpoint is designed for programmatic access and automation scripts.
*   **Parameters:**
    *   `ip` - IPv4 or IPv6 address (will be canonicalized)
*   **Response Format (Blocked IP):**
    ```json
    {
      "node": "follower-2",
      "status": "blocked",
      "actors": 2,
      "chains": {
        "SimpleBlockChain": "2025-11-22T02:00:00Z",
        "API-Abuse-Chain": "2025-11-22T01:30:00Z"
      },
      "earliest_block": "2025-11-22T01:00:00Z",
      "latest_expiry": "2025-11-22T02:00:00Z",
      "cluster_hint": "http://leader:8080/cluster/ip/192.168.1.1"
    }
    ```
*   **Response Format (Unblocked IP):**
    ```json
    {
      "node": "follower-2",
      "status": "unblocked",
      "last_seen": "2025-11-22T01:00:00Z",
      "last_unblock": "2025-11-22T00:30:00Z",
      "unblock_reason": "good-actor:monitoring_agent"
    }
    ```
*   **Response Format (Unknown IP):**
    ```json
    {
      "node": "follower-2",
      "status": "unknown"
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
    *   The `cluster_hint` field (on followers) provides the URL to query for cluster-wide status
*   **Responses:**
    *   `200 OK`: Successfully returns IP status.
    *   `400 Bad Request`: Invalid IP address format.

### **`/cluster/ip/{ip}`** (Leader Only)

*   **Method:** `GET`
*   **Content-Type:** `text/plain; charset=utf-8`
*   **Description:** Returns aggregated IP status across all cluster nodes in human-readable plain text format. This endpoint queries all nodes in the cluster and provides a comprehensive view of the IP's status across the entire deployment. Only available on leader nodes.
*   **Parameters:**
    *   `ip` - IPv4 or IPv6 address (will be canonicalized)
*   **Response Format:**
    ```
    cluster_status: blocked
    nodes:
      - name: follower-1
        status: blocked
        actors: 1
        chains:
          - SimpleBlockChain (until: 2025-11-22T02:00:00Z)
        earliest_block: 2025-11-22T01:00:00Z
        latest_expiry: 2025-11-22T02:00:00Z
      - name: follower-2
        status: blocked
        actors: 2
        chains:
          - SimpleBlockChain (until: 2025-11-22T02:00:05Z)
          - API-Abuse-Chain (until: 2025-11-22T01:30:05Z)
        earliest_block: 2025-11-22T01:00:05Z
        latest_expiry: 2025-11-22T02:00:05Z
      - name: follower-3
        status: unblocked
        last_seen: 2025-11-22T00:50:00Z
      - name: follower-4
        status: error
        error: HTTP 500
    ```
*   **Cluster Status Values:**
    *   `"blocked"`: IP is blocked on all nodes that have information about it
    *   `"unblocked"`: IP is not blocked on any node (but may have been seen)
    *   `"unknown"`: IP is not known to any node
    *   `"mixed"`: IP has different statuses across nodes (e.g., blocked on some, unblocked on others)
*   **Node Status Values:**
    *   `"blocked"`: IP is currently blocked on this node
    *   `"unblocked"`: IP is known but not blocked on this node
    *   `"unknown"`: IP is not known to this node
    *   `"error"`: Failed to query this node (network error, timeout, etc.)
*   **Notes:**
    *   The leader queries all nodes concurrently with a 5-second timeout per node
    *   Nodes that fail to respond are marked with `"status": "error"` and include an error message
    *   This endpoint provides the most complete view of an IP's status across the cluster
*   **Responses:**
    *   `200 OK`: Successfully returns aggregated IP status.
    *   `400 Bad Request`: Invalid IP address format.
    *   `404 Not Found`: This endpoint is only available on leader nodes.

### **`/api/v1/cluster/ip/{ip}`** (Leader Only)

*   **Method:** `GET`
*   **Content-Type:** `application/json`
*   **Description:** Returns aggregated IP status across all cluster nodes in JSON format. This is the JSON version of `/cluster/ip/{ip}`, designed for programmatic access. Only available on leader nodes.
*   **Parameters:**
    *   `ip` - IPv4 or IPv6 address (will be canonicalized)
*   **Response Format:**
    ```json
    {
      "cluster_status": "blocked",
      "nodes": [
        {
          "name": "follower-1",
          "status": "blocked",
          "actors": 1,
          "chains": {
            "SimpleBlockChain": "2025-11-22T02:00:00Z"
          },
          "earliest_block": "2025-11-22T01:00:00Z",
          "latest_expiry": "2025-11-22T02:00:00Z"
        },
        {
          "name": "follower-2",
          "status": "blocked",
          "actors": 2,
          "chains": {
            "SimpleBlockChain": "2025-11-22T02:00:05Z",
            "API-Abuse-Chain": "2025-11-22T01:30:05Z"
          },
          "earliest_block": "2025-11-22T01:00:05Z",
          "latest_expiry": "2025-11-22T02:00:05Z"
        },
        {
          "name": "follower-3",
          "status": "unblocked",
          "last_seen": "2025-11-22T00:50:00Z"
        },
        {
          "name": "follower-4",
          "status": "error",
          "error": "HTTP 500"
        }
      ]
    }
    ```
*   **Cluster Status Values:** Same as `/cluster/ip/{ip}`
*   **Node Status Values:** Same as `/cluster/ip/{ip}`
*   **Notes:** Same as `/cluster/ip/{ip}`
*   **Responses:**
    *   `200 OK`: Successfully returns aggregated IP status.
    *   `400 Bad Request`: Invalid IP address format.
    *   `404 Not Found`: This endpoint is only available on leader nodes.

### **`/api/v1/cluster/internal/ip/{ip}`** (Internal Use)

*   **Method:** `GET`
*   **Content-Type:** `application/json`
*   **Description:** Internal endpoint used by the leader to query follower nodes for IP status. This endpoint returns the same information as `/api/v1/ip/{ip}` but without the `node` and `cluster_hint` fields, as these are added by the leader during aggregation. This endpoint is not intended for direct user access.
*   **Parameters:**
    *   `ip` - IPv4 or IPv6 address (will be canonicalized)
*   **Response Format:** Same as `/api/v1/ip/{ip}` but without `node` and `cluster_hint` fields.
*   **Responses:**
    *   `200 OK`: Successfully returns IP status.
    *   `400 Bad Request`: Invalid IP address format.

---

## Usage Examples

### Query IP status on local node (plain text)
```bash
curl http://localhost:8080/ip/192.168.1.100
```

### Query IP status on local node (JSON)
```bash
curl http://localhost:8080/api/v1/ip/192.168.1.100
```

### Query IP status across cluster (leader only)
```bash
curl http://leader:8080/cluster/ip/192.168.1.100
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
