# **Bot-Detector: Behavioral Threat Mitigation**

Bot-Detector is a high-performance Go application designed to monitor live access logs, identify malicious or anomalous behavior using configurable behavioral chains, and dynamically block offending IP addresses via the HAProxy Runtime API.

## **Features**

* **Real-Time Behavioral Analysis:** Uses flexible YAML configurations to detect sequential patterns (e.g., initial probe, specific request, failed login).
* **HAProxy Integration:** Executes immediate IP blocking via the HAProxy Runtime Socket.
* **High Resilience:** Automatically handles HAProxy socket unavailability by switching the action from block to log for the duration of the outage (**Passive Monitoring Mode**).
* **Log Rotation Safe:** Continuously tails live log files, automatically detecting and re-opening the file after log rotation events (e.g., logrotate).
* **Graceful Shutdown:** Implements signal handlers (SIGINT, SIGTERM) for safe, controlled process termination.
* **Dry Run Mode:** Allows testing behavioral chains against static log files without affecting a live HAProxy instance.
* **Memory Optimization:** Automatically purges state for IPs that are no longer relevant for time-based rules, minimizing memory footprint.

## **Setup and Usage**

### **Step 1: HAProxy Configuration (CRITICAL)**

The bot-detector writes IP block information via HAProxy runtime API and use stick tables, but **HAProxy must be configured to read it and act on it**.
See details in [HaproxySetup.md](HaproxySetup.md)


### **Step 2: Running the Bot-Detector**

The application is configured using command-line flags.

#### **Production Mode (Live Tailing)**

Run the application pointing to your live log file, HAProxy socket, and map file.

```bash
./bot-detector \
    -log-path "/var/log/http/access.log" \
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
go build -o bot-detector .
```

This will produce a single executable named `bot-detector`.

## **Command-Line Flags (Execution)**

| Flag | Default | Description |
| :--- | :--- | :--- |
| **`--log-path`** | `/var/log/http/access.log` | Path to the live access log file to tail (ignored in dry-run). |
| **`--poll-interval`** | `5s` | Interval to check the YAML file for changes (ignored in dry-run). |
| **`--cleanup-interval`**| `1m` | Interval to run the routine that cleans up idle IP state. |
| **`--idle-timeout`** | `30m` | Duration an IP must be inactive before its state is purged from memory. |
| **`--log-level`** | `warning` | Set minimum log level to display. *See Log Levels below.* |
| **`--dry-run`** | `false` | If true, runs in test mode, ignoring HAProxy and live logging. |
| **`--test-log`** | `test_access.log` | Path to a static file containing log lines for dry-run testing. |

---

### **Log Levels**

The application uses a unified logging system with five discrete levels. The `--log-level` flag controls the minimum severity level that will be displayed in the output.

| Level | Severity | Description |
| :--- | :--- | :--- |
| **`critical`** | **0** (Highest) | Only displays actions that modify state or terminate the program (e.g., **IP blocks**, graceful **SHUTDOWN**). |
| **`error`** | **1** | Displays severe, non-fatal issues (e.g., file read errors, **HAProxy connection failures** that trigger fail-safe). |
| **`warning`** | **2** (Default) | Includes non-critical operational issues that should be reviewed (e.g., failed timestamp parsing, malformed URL referrers). |
| **`info`** | **3** | Includes major application lifecycle events (e.g., configuration **LOAD**, **DRYRUN** start/completion, tailing start). |
| **`debug`** | **4** (Lowest) | The most verbose level. Includes high-volume internal logic like individual step **MATCH**,


**Example Command (Recommended for Production):**

```bash
./bot-detector \
--log-path "/var/log/nginx/access.log" \
--yaml-path "test_rules.yaml" \
--idle-timeout "30m"
```

**Example Command (Testing Rules):**

```bash
./bot-detector --dry-run \
--yaml-path "test_rules.yaml" \
--log-level debug \
--test-log "large_test_data.log"
```

# **Behavioral Chains Configuration File (chains.yaml)**

This file defines the sequential behavioral chains used by the bot-detector to identify and act upon suspicious traffic patterns.
The file is structured as a top-level map containing a single key, chains, which holds an array of individual chain definitions.

## **Root Structure**

| Field | Type | Description |
| :---- | :---- | :---- |
| **chains** | array of object | The list of behavioral chains to be loaded. |
| **default_block_duration** | string | Optional. A global block duration to apply to any `block` action chain that does not define its own `block_duration`. Format: Go duration string (e.g., "5m", "1h"). |

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
| **field_matches** | map\[string\]string | Yes | A set of key-value pairs where the key is a field from the log line (e.g., **Method**, **StatusCode**, **Path**, **UserAgent**) and the value is a **Go Regular Expression** that must match the corresponding log entry field. |
| **max_delay** | string | No | **(Steps 2+)** The maximum allowed time between the previous step and this one. If exceeded, the chain resets. Ignored on the first step. Format: Go duration string (e.g., "10s", "1m"). |
| **min_delay**	| string | No | **(Steps 2+)** The minimum required time between the *previous successful step in this chain* and the current step. If not met, the chain resets. Ignored on the first step. Format: Go duration string (e.g., "10s", "1m"). |
| **first_hit_since** | string | No | **(First Step Only)** The first step will only match if the *last overall request* for the associated tracking key (IP or IP+UA) occurred *within* this duration. If the last request was too long ago, or if the IP has never been seen, the first step will not match. Useful for detecting rapid-fire activity from a known actor. Format: Go duration string (e.g., "30m", "12h"). |

### `field_matches`

| Field | Type | Description |
| :--- | :--- | :--- |
| **IP** | `string` | The client IP address. |
| **Method** | `string` | The HTTP request method (e.g., `GET`, `POST`). |
| **Path** | `string` | The requested URL path. |
| **StatusCode** | `int` | The HTTP response status code (e.g., `200`, `404`). |
| **Referrer** | `string` | The full HTTP Referer header value. Use a regular expression to match specific parts, such as the path (e.g., `^https?://[^/]+/login$`). |
| **UserAgent** | `string` | The HTTP User-Agent header value. |

---

## **Memory Management and State Cleanup**

The bot-detector holds the state of IPs in memory. To prevent memory from growing indefinitely, two cleanup mechanisms are in place:

1.  **Idle Timeout (`--idle-timeout`):** An IP's state is purged if it has been inactive (no requests seen) for longer than this duration. This is the general-purpose cleanup for all IPs.

2.  **`first_hit_since` Optimization:** If your configuration uses `first_hit_since` rules, the application performs a more aggressive cleanup. It calculates the longest `first_hit_since` duration across all your chains. If an IP's last request is older than this duration, and it's not in the middle of a chain, its state is purged immediately, even if it hasn't reached the main `idle-timeout`. This ensures that memory is not wasted on single-hit IPs that can no longer trigger a time-based rule. If no chains use `first_hit_since`, this optimization is disabled, and state is only stored for IPs that are actively progressing in a chain.


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
      - max_delay: "5m"
        field_matches:
          Method: "POST"
          Path: "^/api/login$"
          # Must result in a 401 Unauthorized
          StatusCode: "^401$"

      - max_delay: "10s"
        field_matches:
          Method: "POST"
          Path: "^/api/login$"
          StatusCode: "^401$"

      - max_delay: "5s"
        field_matches:
          Method: "POST"
          Path: "^/api/login$"
          StatusCode: "^401$"

  # 2. CHAIN: Content Scraper (Log Only)
  - name: Content-Scraper-Fast
    action: log
    steps:
      - max_delay: "5m"
        field_matches:
          Method: "GET"
          # Request an article page
          Path: "^/article/\\d+$"
          # User agent might be suspicious
          UserAgent: "(?i)(curl|wget|python-requests)"

      - max_delay: "1s"
        field_matches:
          Method: "GET"
          # Request another article page very quickly
          Path: "^/article/\\d+$"

      - max_delay: "1s"
        field_matches:
          Method: "GET"
          # Request a third article page very quickly
          Path: "^/article/\\d+$"
```
