# HaProxy setup

Setting up your two-node HAProxy/Bot Detector cluster requires synchronizing the configuration across both hosts: **rex (10.2.2.60)** and **rudi (10.2.2.30)**.

The key to this setup is that the bot detector uses a unified list of HAProxy targets defined in chains.yaml, and its internal logic automatically chooses between the faster **Unix Domain Socket (UDS)** for the local HAProxy and **TCP/IP** for the remote one.

---

## **1\. HAProxy Configuration (haproxy.cfg)**

The `haproxy.cfg` file must be functionally **identical** on both hosts (rex and rudi). It must define the required stick tables and expose both the local Unix socket and a specific TCP port for the remote bot detector to connect.

### **A. Stick Tables and Runtime Socket**

Add the following to the configuration, typically after the defaults section.

Extrait de code

```
# haproxy.cfg (on both rex and rudi)

global
    # ... existing settings ...
    # This exposes the local Unix Domain Socket (UDS) for local control
    stats socket /run/haproxy/admin.sock mode 660 level admin expose-fd listeners

# --- Bot Detector Stick Tables ---
# Defined duration tables matching the names in chains.yaml
stick-table type ip size 2m expire 1h store gpc0 name table_1h
stick-table type ip size 500k expire 5m store gpc0 name table_5m

# --- TCP Runtime API Listener (for remote Bot Detector) ---
# This opens the TCP port for control. Replace 10.2.2.60/10.2.2.30 with the
# actual local IP address if the host has multiple IPs.
listen runtime_api
    bind 10.2.2.60:9999   # On rex (10.2.2.60)
    # OR
    # bind 10.2.2.30:9999 # On rudi (10.2.2.30)

    mode tcp
    maxconn 10 # Limit control connections
    timeout client 10s
    timeout server 10s

    # SECURITY: Restrict access to only the other HAProxy host (and localhost)
    acl allowed_control src 10.2.2.60 10.2.2.30 127.0.0.1
    tcp-request content reject unless allowed_control
```

*Note: The bind address should be the local host's IP address (10.2.2.60 on rex, 10.2.2.30 on rudi) or 0.0.0.0 if you prefer to bind all interfaces.*

### **B. Blocking Rules**

Add the rejection rule to all relevant frontends (tcpforward\_http and tcpforward\_https) to reject traffic if the source IP is found in either stick table.

Extrait de code

```
# haproxy.cfg (on both rex and rudi)

frontend tcpforward_http from base
    # ... existing settings ...
    # CRITICAL: Place this as the first tcp-request connection rule
    tcp-request connection reject if { src,table_5m } or { src,table_1h }
    # ... rest of frontend rules

frontend tcpforward_https from base
    # ... existing settings ...
    tcp-request connection reject if { src,table_5m } or { src,table_1h }
    # ... rest of frontend rules
```

---

## **2\. Bot Detector Configuration (chains.yaml)**

This file is the **master list** of all targets and must be **identical** on both hosts. The haproxy\_addresses list specifies every endpoint the bot detector must communicate with.

YAML

```yaml
# chains.yaml (Identical on both rex and rudi)

version: "1.0"
# ... other config ...

# --- Block Duration Mapping ---
duration_tables:
    5m: table_5m # Matches the stick-table name in haproxy.cfg
    1h: table_1h # Matches the stick-table name in haproxy.cfg
default_block_duration: "5m"

# --- HAProxy Target Addresses ---
# This list contains ALL HAProxy control endpoints across the cluster.
# The bot detector handles the connection type (Unix vs. TCP) automatically.
haproxy_addresses:
  # 1. Local HAProxy (Uses Unix Socket - faster, more secure locally)
  - /run/haproxy/admin.sock

  # 2. Remote HAProxy on rex (Uses TCP/IP)
  - 10.2.2.60:9999

  # 3. Remote HAProxy on rudi (Uses TCP/IP)
  - 10.2.2.30:9999

# ... chains definitions ...
```

---

## **3\. Bot Detector Execution**

The execution command is identical on both hosts, assuming the executable and YAML file paths are the same. Each instance will read the full list of HAProxy addresses from the YAML file, but its local connection will benefit from the Unix socket while its cross-host communication will use TCP.

| Host | IP | Execution Command (Example) |
| :---- | :---- | :---- |
| **rex** | 10.2.2.60 | ./bot-detector \--yamlFilePath /etc/bot-detector/chains.yaml \--logFilePath /var/log/haproxy/access.log |
| **rudi** | 10.2.2.30 | ./bot-detector \--yamlFilePath /etc/bot-detector/chains.yaml \--logFilePath /var/log/haproxy/access.log |

**Post-Setup Verification:**
When a bot detector on **rex** needs to block an IP:

1. It attempts to block via /run/haproxy/admin.sock (Local HAProxy \- **UDS**).
2. It attempts to block via 10.2.2.60:9999 (The TCP endpoint for rex \- **TCP**).
3. It attempts to block via 10.2.2.30:9999 (Remote HAProxy on rudi \- **TCP**).

Due to the nature of concurrent execution, it only needs one success per host. The local UDS attempt will typically succeed first and is included for completeness and speed.
