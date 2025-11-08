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

The application is configured using a YAML file and a few command-line flags.

#### **Production Mode (Live Tailing)**

```bash
./bot-detector \
    --log-path "/var/log/http/access.log" \
    --yaml-path "chains.yaml"
```

#### **Dry Run Mode (Testing)**

Use `-dry-run` to test your chains against a static log file. This will process the file once and log all match actions without attempting to connect to HAProxy.

```bash
# test_access.log contains the log lines you want to test
./bot-detector --dry-run --test-log "test_access.log" --yaml-path "chains.yaml"
```

## **Resilience and Logging**

### **Passive Monitoring Mode (HAProxy Fail-Safe)**

If an HAProxy is unavailable during a block attempt (e.g., HAProxy is restarting or down), the program will immediately log the connection error and **downgrade the action to log** for that event. It will continue attempting the block for subsequent events.

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
| **`--yaml-path`** | `chains.yaml` | Path to the YAML configuration file. |
| **`--log-path`** | `/var/log/http/access.log` | Path to the live access log file to tail (ignored in dry-run). |
| **`--dry-run`** | `false` | If true, runs in test mode, ignoring HAProxy and live logging. |
| **`--test-log`** | `test_access.log` | Path to a static file containing log lines for dry-run testing. |

---

### **Log Levels (in `chains.yaml`)**

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
```

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
| **version** | string | The configuration version. Must match a supported version (e.g., "1.0"). |
| **chains** | array of object | The list of behavioral chains to be loaded. |
| **log_level** | string | Optional. Set minimum log level: `critical`, `error`, `warning`, `info`, `debug`. Default: `warning`. |
| **poll_interval** | string | Optional. Interval to check this file for changes. Default: `5s`. |
| **cleanup_interval**| string | Optional. Interval to run the routine that cleans up idle IP state. Default: `1m`. |
| **idle_timeout** | string | Optional. Duration an IP must be inactive before its state is purged. Default: `30m`. |
| **out_of_order_tolerance** | string | Optional. Maximum duration an out-of-order log entry will be processed. Default: `5s`. |
| **default_block_duration** | string | Optional. A global block duration to apply to any `block` action chain that does not define its own `block_duration`. Format: Go duration string (e.g., "5m", "1h"). |
| **haproxy_max_retries** | int | Optional. Number of attempts to send a command to an HAProxy instance. Default: `3`. |
| **haproxy_retry_delay** | string | Optional. Duration to wait between retry attempts. Default: `200ms`. |
| **haproxy_dial_timeout** | string | Optional. Timeout for establishing a connection to an HAProxy socket. Default: `5s`. |

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
| **field_matches** | map | Yes | A set of key-value pairs defining the conditions for the step to match. See the `field_matches` section below for details on the powerful new syntax. |
| **max_delay** | string | No | **(Steps 2+)** The maximum allowed time between the previous step and this one. If exceeded, the chain resets. Ignored on the first step. Format: Go duration string (e.g., "10s", "1m"). |
| **min_delay**	| string | No | **(Steps 2+)** The minimum required time between the *previous successful step in this chain* and the current step. If not met, the chain resets. Ignored on the first step. Format: Go duration string (e.g., "10s", "1m"). |
| **min_time_since_last_hit** | string | No | **(First Step Only)** The first step will only match if the time since the *last overall request* from the same tracking key (IP or IP+UA) is **greater than** this duration. If the last request was too recent, or if the IP has never been seen before, the step will not match. This is useful for detecting "sleepy" bots that have long periods of inactivity between requests, helping to distinguish them from normal user traffic. Format: Go duration string (e.g., "30m", "12h"). |

### `field_matches`

| Field | Type | Description |
| :--- | :--- | :--- |
| **IP** | `string` | The client IP address. |
| **Method** | `string` | The HTTP request method (e.g., `GET`, `POST`). |
| **Path** | `string` | The requested URL path. |
| **StatusCode** | `int` | The HTTP response status code (e.g., `200`, `404`). |
| **Referrer** | `string` | The full HTTP Referer header value. Use a regular expression to match specific parts, such as the path (e.g., `^https?://[^/]+/login$`). |
| **UserAgent** | `string` | The HTTP User-Agent header value. |

### **Advanced `field_matches` Syntax**

The `field_matches` block supports a flexible syntax for defining match conditions, making your rules both powerful and easy to read.

#### **1. Simple Values (Shorthand)**

The simplest match is a direct value. The parser intelligently determines the match type.

*   **Exact String Match (Default for strings):**
    ```yaml
    Method: "POST"
    ```
*   **Exact Integer Match (Default for numbers):**
    ```yaml
    StatusCode: 404
    ```

#### **2. Prefixed String Matchers**

For more complex string matching, use a prefix.

*   **Regular Expression:**
    ```yaml
    UserAgent: "regex:(?i)(bot|crawler|python)"
    ```
*   **Status Code Pattern:** A special shorthand for matching status code classes.
*   **File-Based Matcher:**
    ```yaml
    # Contents of bad_user_agents.txt:
    # BadBot/1.0
    # regex:(?i)evil-crawler
    UserAgent: "file:./bad_user_agents.txt"
    ```
    ```yaml
    StatusCode: "4XX" # Matches 400-499
    ```

#### **3. List of Values (OR Condition)**

Provide a list to match if the field's value is **any of** the items in the list. You can mix match types within a list.

```yaml
field_matches:
  Method: ["POST", "PUT"]
  UserAgent: ["file:./bad_user_agents.txt", "SpecificBadBot/2.0"] # Mix file and direct values
  StatusCode: [401, 403, "5XX"] # Matches 401, 403, or any 5xx code
  Path:
    - "/login"
    - "regex:^/reset-password/\\w+$"
```

#### **4. Object for Numeric Ranges (AND Condition)**

Use an object to define numeric ranges. This is especially useful for `StatusCode`. All conditions in the object must be met.

*   `gt`: greater than
*   `gte`: greater than or equal to
*   `lt`: less than
*   `lte`: less than or equal to

```yaml
field_matches:
  # Matches any status code from 401 to 499 (inclusive)
  StatusCode:
    gte: 401
    lt: 500
```

---

## **Memory Management and State Cleanup**

The bot-detector holds the state of IPs in memory. To prevent memory from growing indefinitely, two cleanup mechanisms are in place:

1.  **Idle Timeout (`--idle-timeout`):** An IP's state is purged if it has been inactive (no requests seen) for longer than this duration. This is the general-purpose cleanup for all IPs.

2.  **`min_time_since_last_hit` Optimization:** If your configuration uses `min_time_since_last_hit` rules, the application performs a more aggressive cleanup. It calculates the longest `min_time_since_last_hit` duration across all your chains. If an IP's last request is older than this duration, and it's not in the middle of a chain, its state is purged immediately, even if it hasn't reached the main `idle-timeout`. This ensures that memory is not wasted on IPs that can no longer trigger a time-based rule. If no chains use this rule, this optimization is disabled.


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
          Path: "/api/login"
          # Must result in a 401 Unauthorized
          StatusCode: 401

      - max_delay: "10s"
        field_matches:
          Method: "POST"
          Path: "/api/login"
          StatusCode: 401

      - max_delay: "5s"
        field_matches:
          Method: "POST"
          Path: "/api/login"
          StatusCode: 401

  # 2. CHAIN: Content Scraper (Log Only)
  - name: Content-Scraper-Fast
    action: log
    steps:
      - max_delay: "5m"
        field_matches:
          Method: "GET"
          # Request an article page
          Path: "regex:^/article/\\d+$"
          # User agent might be suspicious
          UserAgent: "regex:(?i)(curl|wget|python-requests)"

      - max_delay: "1s"
        field_matches:
          Method: "GET"
          # Request another article page very quickly
          Path: "regex:^/article/\\d+$"

      - max_delay: "1s"
        field_matches:
          Method: "GET"
          # Request a third article page very quickly
          Path: "regex:^/article/\\d+$"
```
