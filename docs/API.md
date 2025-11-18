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
