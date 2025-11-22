# HAProxy Setup

This document details how to configure [HAProxy](https://www.haproxy.org/) to work with the bot-detector.

The key to this setup is that the bot detector uses a unified list of HAProxy targets defined in `config.yaml`, and its internal logic automatically chooses between the faster **Unix Domain Socket (UDS)** for the local HAProxy and **TCP/IP** for the remote one.


## 1. Core Concepts

For the bot-detector to work, HAProxy must be configured to do three things:
1.  **Listen for commands:** Expose a Runtime API endpoint so the bot-detector can send `block` and `unblock` instructions.
2.  **Remember bad IPs:** Use `stick-tables` to store the list of blocked IP addresses and for how long they should be blocked.
3.  **Deny traffic:** Use Access Control Lists (`ACLs`) to check incoming requests against the stick-tables and deny access if an IP is on the list.

### A. Exposing the Runtime API (`stats socket`)

The bot-detector communicates with HAProxy via its Runtime API. You must expose this API in the `global` section of your `haproxy.cfg`.

*   **For local communication (recommended):** Use a Unix socket. It's faster and more secure.
    ```haproxy
    global
        stats socket /run/haproxy/admin.sock mode 660 level admin
    ```
*   **For remote communication:** Use a TCP socket. This is necessary if the bot-detector runs on a different machine than HAProxy.
    ```haproxy
    global
        stats socket ipv4@127.0.0.1:9999 level admin
    ```

### B. Defining Stick Tables (HAProxy's Memory)

Think of HAProxy's **stick-tables** as temporary in-memory lists where HAProxy can remember information about clients. The bot-detector uses these tables to tell HAProxy which IPs to block. They are defined inside `backend` blocks.

```haproxy
backend table_1h_ipv4
	stick-table type ip size 2m expire 1h store gpc0 peers mypeers
```

Here's what each part means:
*   **`backend table_1h_ipv4`**: A "pseudo" backend that exists only to hold the stick-table definition. The name must match what the bot-detector expects (e.g., `table_1h` from `config.yaml` + `_ipv4`).
*   **`type ip`**: Tells HAProxy to store IPv4 addresses. Use `type ipv6` for IPv6.
*   **`size 2m`**: The maximum number of entries the table can hold (e.g., 2 million).
*   **`expire 1h`**: How long an entry will remain in the table before HAProxy automatically removes it. This duration should match the table's purpose (e.g., 1 hour).
*   **`store gpc0`**: This is the key flag. It tells HAProxy to store a "General Purpose Counter" for each IP. The bot-detector sets this counter to `1` to mark an IP as blocked.
*   **`peers mypeers`**: If you have multiple HAProxy servers in a cluster, this enables synchronization of the table's contents between them.

### C. Creating ACLs and Denying Requests

In your `frontend`, you need rules to check the stick-tables for each incoming request. This is done with **ACLs** (Access Control Lists).

1.  **Define the ACL:**
    ```haproxy
    acl blocked_1h_ipv4 src_get_gpc0(table_1h_ipv4) gt 0
    ```
    This line creates a rule named `blocked_1h_ipv4` that is `true` if the client's source IP (`src`) is found in `table_1h_ipv4` and its `gpc0` counter is greater than 0.

2.  **Use the ACL to block traffic:**
    ```haproxy
    http-request deny deny_status 429 if blocked_1h_ipv4 or blocked_1h_ipv6
    ```
    This rule tells HAProxy to deny the request with a `429 Too Many Requests` status if any of the "blocked" ACLs are true.

### D. Synchronizing State Across a Cluster (`peers`)

If you run multiple HAProxy instances in a cluster for high availability, you need to synchronize their stick-tables. This ensures that when the bot-detector blocks an IP on one node, all other nodes in the cluster also block that IP. This is achieved using the `peers` section.

*   **`peers <name>`**: The `peers mypeers` block defines a group of servers that will communicate with each other to share state.

*   **`peer <name> <address>:<port>`**: Each `peer` line within the block defines a member of the group. You should list all HAProxy nodes in your cluster here. The port (e.g., `10000`) must be open for TCP traffic between the nodes.

*   **`localpeer <name>`**: In the `global` section, the `localpeer node1` directive identifies which node the current configuration file belongs to. **This value must be changed on each server** to match its name in the `peers` list (e.g., `localpeer node2` on the second server).

*   **Enabling Synchronization**: To activate synchronization for a specific stick-table, you must add the `peers mypeers` keyword to its definition, as shown in the example below.

```haproxy
global
    # ...
    localpeer node1 # This line must be changed on each server in the cluster

peers mypeers
    peer node1 192.168.1.1:10000
    peer node2 192.168.1.2:10000

backend table_1h_ipv4
	stick-table type ip size 2m expire 1h store gpc0 peers mypeers
```


## 2. Full HAProxy Configuration Example (haproxy.cfg)

Here is a complete `haproxy.cfg` example incorporating the concepts above.

```
# haproxy.cfg

global
    log /dev/log    local0
    log /dev/log    local1 notice
    chroot /var/lib/haproxy
    stats socket /run/haproxy/admin.sock mode 660 level admin
    # for communication with haproxy you may want to use tcp instead of local socket
    # especially with multiple instances
    stats socket ipv4@127.0.0.1:9999  level admin  expose-fd listeners
    stats timeout 30s
    user haproxy
    group haproxy
    daemon

    # Default SSL material locations
    ca-base /etc/ssl/certs
    crt-base /etc/ssl/private

    # See: https://ssl-config.mozilla.org/#server=haproxy&server-version=2.0.3&config=intermediate
    ssl-default-bind-ciphers ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-ECDSA-CHACHA20-POLY1305:ECDHE-RSA-CHACHA20-POLY1305:DHE-RSA-AES128-GCM-SHA256:DHE-RSA-AES256-GCM-SHA384
    ssl-default-bind-ciphersuites TLS_AES_128_GCM_SHA256:TLS_AES_256_GCM_SHA384:TLS_CHACHA20_POLY1305_SHA256
    ssl-default-bind-options ssl-min-ver TLSv1.2 no-tls-tickets

    # see peers section below, change depending on which node this config is
    localpeer node1

peers mypeers
    # to save stick tables between restarts
    peer node1 192.168.1.1:10000
    peer node2 192.168.1.2:10000

defaults
    log    global
    mode    http
    option    httplog
    option    dontlognull
    timeout connect 5000
    timeout client  50000
    timeout server  50000
    errorfile 400 /etc/haproxy/errors/400.http
    errorfile 403 /etc/haproxy/errors/403.http
    errorfile 408 /etc/haproxy/errors/408.http
    errorfile 500 /etc/haproxy/errors/500.http
    errorfile 502 /etc/haproxy/errors/502.http
    errorfile 503 /etc/haproxy/errors/503.http
    errorfile 504 /etc/haproxy/errors/504.http

backend table_1h_ipv4
    stick-table type ip size 2m expire 1h store gpc0 peers mypeers

backend table_5m_ipv4
    stick-table type ip size 500k expire 5m store gpc0 peers mypeers

backend table_1h_ipv6
    stick-table type ipv6 size 2m expire 1h store gpc0 peers mypeers

backend table_5m_ipv6
    stick-table type ipv6 size 500k expire 5m store gpc0 peers mypeers

frontend fe_main
    # Listen on both ipv4 & ipv6 (this depends on your setup)
    bind :::80 v4v6

    acl blocked_1h_ipv4 src_get_gpc0(table_1h_ipv4) gt 0
    acl blocked_1h_ipv6 src_get_gpc0(table_1h_ipv6) gt 0
    acl blocked_5m_ipv4 src_get_gpc0(table_5m_ipv4) gt 0
    acl blocked_5m_ipv6 src_get_gpc0(table_5m_ipv6) gt 0

    http-request deny deny_status 429 if blocked_1h_ipv4 or blocked_5m_ipv4 or blocked_1h_ipv6 or blocked_5m_ipv6

    default_backend be_servers

backend be_servers
    mode http
    # dummy error file for testing
    errorfile 503 /tmp/dummy.http
```

For testing, dummy.http:

```
HTTP/1.0 200 Found
Cache-Control: no-cache
Connection: close
Content-Type: text/plain

200 Found
```


## 3. Manual HAProxy Stick Table Operations

You can manually interact with HAProxy stick tables using `socat` for testing or troubleshooting.

### Show all stick tables
```bash
echo "show table" | socat stdio /run/haproxy/admin.sock
```

### Show entries in a specific table
```bash
echo "show table table_1h_ipv4" | socat stdio /run/haproxy/admin.sock
```

### Block an IP (set gpc0=1)
```bash
# IPv4
echo "set table table_1h_ipv4 key 192.168.1.100 data.gpc0 1" | socat stdio /run/haproxy/admin.sock

# IPv6
echo "set table table_1h_ipv6 key 2001:db8::1 data.gpc0 1" | socat stdio /run/haproxy/admin.sock
```

### Unblock but keep IP in table (set gpc0=0)
```bash
echo "set table table_1h_ipv4 key 192.168.1.100 data.gpc0 0" | socat stdio /run/haproxy/admin.sock
```

### Remove an IP from table completely
```bash
echo "clear table table_1h_ipv4 key 192.168.1.100" | socat stdio /run/haproxy/admin.sock
```

### Using TCP socket instead of Unix socket
```bash
echo "show table table_1h_ipv4" | socat stdio TCP:127.0.0.1:9999
```

## 4. Bot Detector Configuration (`config.yaml`)

The bot-detector's YAML configuration file is the **master list** of all targets and should be kept consistent across your cluster. The `blockers.backends.haproxy` section specifies every endpoint the bot detector must communicate with.

```yaml
version: "1.0"

application:
  log_level: "info"
  enable_metrics: true

parser:
  timestamp_format: "02/Jan/2006:15:04:05 -0700"

checker:
  actor_cleanup_interval: "1m"
  actor_state_idle_timeout: "30m"

blockers:
  default_duration: "5m"
  commands_per_second: 10
  command_queue_size: 1000
  dial_timeout: "5s"
  max_retries: 3
  retry_delay: "200ms"

  backends:
    haproxy:
      # All HAProxy control endpoints across the cluster
      addresses:
        - "/run/haproxy/admin.sock"  # Local (Unix socket)
        - "10.2.2.60:9999"            # Remote node 1 (TCP)
        - "10.2.2.30:9999"            # Remote node 2 (TCP)

      # Maps block durations to HAProxy stick table names
      duration_tables:
        "5m": "table_5m"
        "1h": "table_1h"

chains:
  # ... chain definitions ...
```
