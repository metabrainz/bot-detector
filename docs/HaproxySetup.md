# HaProxy setup

This document details how to configure HAProxy to work with the bot-detector.

The key to this setup is that the bot detector uses a unified list of HAProxy targets defined in config.yaml, and its internal logic automatically chooses between the faster **Unix Domain Socket (UDS)** for the local HAProxy and **TCP/IP** for the remote one.

---

## **1\. HAProxy Configuration (haproxy.cfg)**

It must define the required stick tables and expose a runtime API socket (either a local Unix socket or a TCP port) for the bot detector to connect to.

### **A. Stick Tables and Runtime Socket**

Add the following to the configuration, typically after the defaults section.

Extrait de code

```
# haproxy.cfg

global
	log /dev/log	local0
	log /dev/log	local1 notice
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

    # see peears section below, change depending on which node this config is
    localpeer node1

peers mypeers
    # to save stick tables between restarts
    peer node1 192.168.1.1:10000
    peer node2 192.168.1.2:10000

defaults
	log	global
	mode	http
	option	httplog
	option	dontlognull
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

# Stick tables are defined in "pseudo" backends. This is a standard HAProxy practice.
# - A separate backend is needed for each unique combination of IP version (IPv4/IPv6) and expiration time.
# - The bot-detector automatically appends "_ipv4" or "_ipv6" to the base table names defined in chains.yaml.
# - `store gpc0` instructs HAProxy to store the General Purpose Counter 0 for each entry.
#   The bot-detector sets this counter to 1 to signify a block.
# - `peers mypeers` enables synchronization of stick table contents across multiple HAProxy nodes.
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

  # Declare one ACL per stick table.
  # `src_get_gpc0(...)` fetches the value of the gpc0 counter for the client's source IP.
  # The ACL is true if the counter is greater than 0 (i.e., the IP is marked as blocked).
  acl blocked_1h_ipv4 src_get_gpc0(table_1h_ipv4) gt 0
  acl blocked_1h_ipv6 src_get_gpc0(table_1h_ipv6) gt 0
  acl blocked_5m_ipv4 src_get_gpc0(table_5m_ipv4) gt 0
  acl blocked_5m_ipv6 src_get_gpc0(table_5m_ipv6) gt 0
  
  # For layer 4 blocking (faster, but less informative to the client), you could use:
  # tcp-request connection reject if blocked_1h_ipv4 or blocked_5m_ipv4 or blocked_1h_ipv6 or blocked_5m_ipv6
  
  # here we return a 429 on http request if the client src IP was blocked
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

---

## **2\. Bot Detector Configuration (config.yaml)**

The bot-detector's YAML configuration file is the **master list** of all targets and should be kept consistent across your cluster. The `blocker_addresses` list specifies every endpoint the bot detector must communicate with.

```yaml
# Example config.yaml
version: "1.0" # This version field is mandatory
# ... other config ...

# --- Block Duration Mapping ---
duration_tables:
    5m: table_5m # Matches the stick-table name in haproxy.cfg without _ipv4/_ipv6 suffix
    1h: table_1h # Matches the stick-table name in haproxy.cfg without _ipv4/_ipv6 suffix
default_block_duration: "5m"

# --- HAProxy Target Addresses ---
# This list contains ALL HAProxy control endpoints across the cluster.
# The bot detector handles the connection type (Unix vs. TCP) automatically.
blocker_addresses:
  # 1. Local HAProxy (Uses Unix Socket - faster, more secure locally)
  - /run/haproxy/admin.sock

  # 2. Remote HAProxy on rex (Uses TCP/IP)
  - 10.2.2.60:9999

  # 3. Remote HAProxy on rudi (Uses TCP/IP)
  - 10.2.2.30:9999

# --- HAProxy Client Settings (Optional) ---
# These settings control the behavior of the bot-detector when it communicates
# with the HAProxy instances listed above.
blocker_max_retries: 3
blocker_retry_delay: "200ms"
blocker_dial_timeout: "5s"
blocker_command_queue_size: 1000
blocker_commands_per_second: 10


# ... chains definitions ...
```
