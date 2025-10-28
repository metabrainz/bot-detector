# **Bot-Detector: Behavioral Threat Mitigation**

Bot-Detector is a high-performance Go application designed to monitor live access logs, identify malicious or anomalous behavior using configurable behavioral chains, and dynamically block offending IP addresses via the HAProxy Runtime API.

## **Features**

* **Real-Time Behavioral Analysis:** Uses flexible YAML configurations to detect sequential patterns (e.g., initial probe, specific request, failed login).  
* **HAProxy Integration:** Executes immediate IP blocking via the HAProxy Runtime Socket.  
* **High Resilience:** Automatically handles HAProxy socket unavailability by switching the action from block to log for the duration of the outage (**Passive Monitoring Mode**).  
* **Log Rotation Safe:** Continuously tails live log files, automatically detecting and re-opening the file after log rotation events (e.g., logrotate).  
* **Graceful Shutdown:** Implements signal handlers (SIGINT, SIGTERM) for safe, controlled process termination.  
* **Dry Run Mode:** Allows testing behavioral chains against static log files without affecting a live HAProxy instance.

## **Setup and Usage**

### **Step 1: HAProxy Configuration (CRITICAL)**

The bot-detector writes IP block information to a map file, but **HAProxy must be configured to read it and act on it**.  
You must integrate the following rules into your /etc/haproxy/haproxy.cfg file.

#### **A. Define the Dynamic Map (In global section)**

Add the following directive to the global section to declare the map file and enable HAProxy to monitor it for updates every 5 seconds.

```
global  
    # ... your existing global settings ...

    # Declare the dynamic map file managed by bot-detector  
    map-file blocked_ips_list /etc/haproxy/maps/blocked_ips.map check 5s
```

#### **B. Implement the Blocking Rule (In Frontends)**

Place these three lines in your primary HTTP and HTTPS frontends, **before** any other static tcp-request content reject rules for highest priority.  

```
frontend tcpforward_http from base  
    # ... existing bind and expect-proxy rules ...

    # --- DYNAMIC BOT-DETECTOR BLOCK ---  
    acl is_blocked src -f blocked_ips_list  
    tcp-request content reject if is_blocked  
    # ----------------------------------

    # ... remaining whitelist/blocklist rules ...
```

#### **C. Finalize Setup**

1. **Create Map File:** Ensure the map file exists and is empty:

```bash
   mkdir -p /etc/haproxy/maps  
   touch /etc/haproxy/maps/blocked_ips.map
```

3. **Permissions:** Set file permissions so the user running bot-detector can **write** to the map file, and the user running haproxy can **read** it.  
4. **Reload HAProxy:** Safely reload your HAProxy configuration.

### **Step 2: Running the Bot-Detector**

The application is configured using command-line flags.

#### **Production Mode (Live Tailing)**

Run the application pointing to your live log file, HAProxy socket, and map file.  

```bash
./bot-detector \  
    -log-path "/var/log/http/access.log" \
    -socket-path "/run/haproxy/admin.sock" \
    -map-path "/etc/haproxy/maps/blocked_ips.map" \  
    -yaml-path "chains.yaml" \
    -cleanup-interval "5m" \  
    -idle-timeout "1h"
```

#### **Dry Run Mode (Testing)**

Use `-dry-run` to test your chains against a static log file. This will process the file once and log all match actions without attempting to connect to HAProxy.  

```bash
# test_access.log contains the log lines you want to test  
./bot-detector -dry-run -test-log "test_access.log" -yaml-path "chains.yaml"
```

## **Resilience and Logging**

### **Passive Monitoring Mode (HAProxy Fail-Safe)**

If the HAProxy socket (`-socket-path`) is unavailable during a block attempt (e.g., HAProxy is restarting or down), the program will immediately log the connection error and **downgrade the action to log** for that event. It will continue attempting the block for subsequent events.  

### **Log Rotation Handling**

The bot-detector monitors the unique file identifier (inode) of the log file. If the file is renamed or truncated (as happens during logrotate), the application detects the change, closes the old handle, and re-opens the new log file to ensure continuous log processing.

## ⚙️ Building the Application

To compile the source code, you must first initialize the Go module and fetch the external dependencies (specifically `gopkg.in/yaml.v3`).

1. **Initialize the Go Module:**

```bash
go mod init bot_detector
```

2. **Fetch Dependencies:**

```bash
go mod tidy
```

3. **Build the Executable:**

```bash
go build -o bot-detector main.go
```

This will produce a single executable named `bot-detector`.

## 🚀 Command Line Usage

The application supports two primary modes: **Production** (live blocking) and **Dry Run** (testing rules against a static log file).

### 1. Production Mode

Production mode tails a live log file, monitors rule changes, cleans up idle state, and sends block commands to HAProxy. This mode often requires `sudo` or root privileges to access log files and the HAProxy socket.

| **Flag** | **Description** | **Default** | 
| ----- | ----- | ----- | 
| `--log-path` | Path to the live access log file to tail. | `/var/log/http/access.log` | 
| `--socket-path` | Path to the HAProxy Runtime API Unix socket. | `/var/run/haproxy.sock` | 
| `--map-path` | Path to the HAProxy map file (`blocked_ips.map`) to update. | `/etc/haproxy/maps/blocked_ips.map` | 
| `--yaml-path` | Path to the behavioral rules configuration. | `chains.yaml` | 
| `--cleanup-interval` | How often to run the memory cleanup routine. | `1m` (1 minute) | 
| `--idle-timeout` | Duration an IP must be inactive before its state is purged (leak prevention). | `30m` (30 minutes) | 

**Example Command (Recommended for Production):**

```bash
./bot-detector \
--log-path "/var/log/nginx/access.log" \  
--socket-path "/var/run/haproxy/admin.sock" \  
--map-path "/etc/haproxy/maps/blocked_ips.map" \  
--idle-timeout "30m"
```

### 2. Dry Run Mode

Dry Run mode is for testing your behavioral rules without connecting to HAProxy or running live cleanup routines.

| **Flag** | **Description** | **Default** | 
 | ----- | ----- | ----- | 
| `--dry-run` | Activates test mode (skips HAProxy and live tailing). | `false` | 
| `--test-log` | Path to a static log file to process lines from. | `test_access.log` | 

**Example Command (Testing Rules):**

```bash
./bot-detector --dry-run \  
--yaml-path "test_rules.yaml" \
--test-log "large_test_data.log"
```

# **Behavioral Chains Configuration File (chains.yaml)**

This file defines the sequential behavioral chains used by the bot-detector to identify and act upon suspicious traffic patterns.  
The file is structured as a top-level map containing a single key, chains, which holds an array of individual chain definitions.

## **Root Structure**

| Field | Type | Description |
| :---- | :---- | :---- |
| chains | array of object | The list of behavioral chains to be loaded. |

## **BehavioralChain Definition (Top Level)**

Each item in the chains array must conform to the following structure:

| Field | Type | Required | Description |
| :---- | :---- | :---- | :---- |
| **name** | string | Yes | A unique, descriptive name for the chain (e.g., API-Abuse-Low-Agent). |
| **steps** | array of object | Yes | The sequential list of steps that define the malicious pattern. |
| **action** | string | Yes | The action to take when the chain is successfully completed by an IP. **Must be one of:** `block` or `log`. |
| **block_duration** | string | No | The duration for which the IP should be blocked if action is block. Format: Go duration string (e.g., "5m", "1h", "30m", "1h30m"). **Required if action is block**. |

## **Step Definition**

Each step in the steps array defines a specific log entry characteristic that must occur in sequence to progress the chain.

| Field | Type | Required | Description |
| :---- | :---- | :---- | :---- |
| **order** | integer | Yes | The sequence number of the step (starting at 1). Steps are processed numerically. |
| **field_matches** | map\[string\]string | Yes | A set of key-value pairs where the key is a field from the log line (e.g., **Method**, **StatusCode**, **Path**, **UserAgent**) and the value is a **Go Regular Expression** that must match the corresponding log entry field. |
| **max_delay** | string | Yes | The maximum allowed time gap between the *previous* successful step and this step. Format: Go duration string (e.g., "10s", "1m"). |

### `field_matches`

| Field | Type | Description |
| :--- | :--- | :--- |
| **IP** | `string` | The client IP address. |
| **Method** | `string` | The HTTP request method (e.g., `GET`, `POST`). |
| **Path** | `string` | The requested URL path. |
| **StatusCode** | `int` | The HTTP response status code (e.g., `200`, `404`). |
| **Referrer** | `string` | The HTTP Referer header value. |
| **ReferrerPrevPath** | `string` | Special, see below |
| **UserAgent** | `string` | The HTTP User-Agent header value. |

#### `ReferrerPrevPath` Field

The `ReferrerPrevPath` field is a **special, internal identifier** used within a `StepDefYAML` configuration to enable a specific type of sequential log matching in a behavioral chain.

##### Purpose

The primary purpose of `ReferrerPrevPath` is to validate that the **current request** originated from a **specific path** visited in the **previous step** of the chain.

It is specifically designed to model typical user flows, such as submitting a form or clicking a navigation link, where the current request's `Referrer` header should logically contain the path of the previous page in the sequence.

##### How it Works

1.  **YAML Definition:** When defining a step in your `chains.yaml`, you specify a regular expression under the key `ReferrerPrevPath`. This regex should describe the expected **path** of the page that should have been visited immediately prior to the current request.

    ```yaml
    # Example YAML snippet for a behavioral chain step:
    steps:
      - order: 2
        field_matches:
          # The regex must match a path component (e.g., "/login").
          # This regex is matched against the full Referrer header value.
          "ReferrerPrevPath": "/login$"
    ```

2.  **Runtime Matching:** When the system processes a log entry for a potential match on this step, it performs the following check:
    * It extracts the **full value of the `Referrer` header** from the current log entry (`entry.Referrer`).
    * It matches this `Referrer` value against the regular expression defined for `ReferrerPrevPath` in the YAML configuration.

##### Key Distinction from `Referrer`

| Field Identifier | Log Entry Value Matched | Regex Intent (Expected Content) |
| :--- | :--- | :--- |
| **`Referrer`** | `entry.Referrer` (The full referrer string) | To match a specific referrer **host, domain, or full URL pattern** (e.g., `^https?://evil\.com/`). |
| **`ReferrerPrevPath`** | **`entry.Referrer`** (The full referrer string) | To match a specific **path component** within the referrer URL (e.g., `/user/profile$`). |

By using `ReferrerPrevPath`, the chain author signals that the regex is path-focused, ensuring the sequential integrity of the bot's observed behavior.
---

## **Example chains.yaml**

This example defines two chains: one for logging suspicious scanning and one for blocking a brute-force-like sequence.  
chains:

```yaml  
  # 1. CHAIN: Credential Stuffing / Brute Force  
  - name: Login-Brute-Force  
    action: block  
    block_duration: "1h"  
    steps:  
      - order: 1  
        max_delay: "5m"  
        field_matches:  
          Method: "POST"  
          Path: "^/api/login$"  
          # Must result in a 401 Unauthorized  
          StatusCode: "^401$" 

      - order: 2  
        max_delay: "10s"  
        field_matches:  
          Method: "POST"  
          Path: "^/api/login$"  
          StatusCode: "^401$" 

      - order: 3  
        max_delay: "5s"  
        field_matches:  
          Method: "POST"  
          Path: "^/api/login$"  
          StatusCode: "^401$" 

  # 2. CHAIN: Content Scraper (Log Only)  
  - name: Content-Scraper-Fast  
    action: log  
    steps:  
      - order: 1  
        max_delay: "5m"  
        field_matches:  
          Method: "GET"  
          # Request an article page  
          Path: "^/article/\\d+$"  
          # User agent might be suspicious  
          UserAgent: "(?i)(curl|wget|python-requests)" 

      - order: 2  
        max_delay: "1s"  
        field_matches:  
          Method: "GET"  
          # Request another article page very quickly  
          Path: "^/article/\\d+$"

      - order: 3  
        max_delay: "1s"  
        field_matches:  
          Method: "GET"  
          # Request a third article page very quickly  
          Path: "^/article/\\d+$"
```
