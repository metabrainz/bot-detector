# **Bot-Detector: Behavioral Threat Mitigation**

Bot-Detector is a high-performance Go application designed to monitor live access logs, identify malicious or anomalous behavior using configurable behavioral chains, and dynamically block offending IP addresses via the HAProxy Runtime API.

## How It Works

The application operates in a continuous loop:

1.  **Tails a log file** (like an HAProxy or Nginx access log) in real-time.
2.  **Parses each new log line** against a configurable regex format defined in the config file.
3.  **Checks the entry** against a series of behavioral chains defined in the YAML configuration file.
4.  **Tracks the state** of each IP address (or IP+User-Agent) as it progresses through these chains.
5.  **Executes an action** (e.g., `block` or `log`) via the HAProxy Runtime API when a chain is completed.
6.  **Manages state** by cleaning up idle or irrelevant IP tracking data to conserve memory.

## **Features**

*   **Real-Time Behavioral Analysis:** Uses flexible YAML configurations to detect sequential patterns.
*   **HAProxy Integration:** Executes immediate IP blocking via the HAProxy Runtime API (TCP or Unix Socket).
*   **High Resilience:** Handles HAProxy instance unavailability by logging the failure and continuing operation.
*   **Configuration Hot-Reload:** Automatically detects and applies changes to the YAML configuration file and its file dependencies without a restart.
*   **Log Rotation Safe:** Continuously tails log files, automatically re-opening the file after log rotation events.
*   **Graceful Shutdown:** Implements signal handlers (SIGINT, SIGTERM) for safe, controlled process termination.
*   **Dry Run Mode:** Allows testing behavioral chains against static log files without affecting a live HAProxy instance.
*   **Memory Optimization:** Automatically purges state for IPs that are no longer relevant, minimizing memory footprint.

## **Setup and Usage**

### **Step 1: HAProxy Configuration (CRITICAL)**

The bot-detector only sends block commands to HAProxy; it does not configure HAProxy itself. For blocking to work, you must configure your HAProxy instance with the necessary **stick tables and ACLs** to act on the information sent by this application.

This is a critical prerequisite. See [HaproxySetup.md](docs/HaproxySetup.md) for a detailed guide and example configuration.


### **Step 2: Running the Bot-Detector**

The application is configured using a YAML file and a few command-line flags.

#### **Production Mode (Live Tailing)**

```sh
./bot-detector \
  --log-path "/var/log/http/access.log" \
  --yaml-path "config.yaml"
```

#### **Dry Run Mode (Testing)**

Use `-dry-run` to test your chains against a static log file. This will process the file once and log all match actions without attempting to connect to HAProxy (even if chain action is block).

```sh
./bot-detector --dry-run \
  --log-path "test_access.log" \
  --yaml-path "config.yaml"
```

## **Resilience and Logging**

### **HAProxy Fail-Safe**

If an HAProxy instance is unavailable during a block or unblock attempt (e.g., it is restarting or down), the program will log the connection error and continue its operation. The command will be attempted on other configured HAProxy instances, and the application will continue to process logs and attempt future blocks. It does not enter a persistent "passive mode"; it simply reports the failure for that specific event.

### **Log Rotation**

The bot-detector monitors the unique file identifier (inode) of the log file. If the file is renamed or truncated (as happens during logrotate), the application detects the change, closes the old handle, and re-opens the new log file to ensure continuous log processing.

## **Building the Application**

To compile the source code, you must first initialize the Go module and fetch the external dependencies (specifically `gopkg.in/yaml.v3`).

1. **Initialize the Go Module:**

```sh
go mod init bot_detector
```

2. **Fetch Dependencies:**

```sh
go mod tidy
```

3. **Build the Executable:**

```sh
go build -o bot-detector .
```

This will produce a single executable named `bot-detector`.

## **Command-Line Flags (Execution)**

| Flag | Default | Description |
| :--- | :--- | :--- |
| **`--yaml-path`** | (none) | **Required.** Path to the YAML configuration file. |
| **`--log-path`** | (none) | **Required.** Path to the access log file to tail (or to read in dry-run mode). |
| **`--dry-run`** | `false` | Optional. If true, runs in test mode, ignoring HAProxy and live logging. |

---

### **Log Levels (in the YAML config file)**

The application uses a unified logging system with five discrete levels. The `--log-level` flag controls the minimum severity level that will be displayed in the output.

| Level | Severity | Description |
| :--- | :--- | :--- |
| **`critical`** | **0** (Highest) | Only displays actions that modify state or terminate the program (e.g., **IP blocks**, graceful **SHUTDOWN**). |
| **`error`** | **1** | Displays severe, non-fatal issues (e.g., file read errors, **HAProxy connection failures** that trigger fail-safe). |
| **`warning`** | **2** (Default) | Includes non-critical operational issues that should be reviewed (e.g., failed timestamp parsing, malformed URL referrers). |
| **`info`** | **3** | Includes major application lifecycle events (e.g., configuration **LOAD**, **DRYRUN** start/completion, tailing start). |
| **`debug`** | **4** (Lowest) | The most verbose level. Includes high-volume internal logic like individual step **MATCH**. |

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
| **timestamp_format** | string | Optional. The time format layout string (per Go's `time.Parse` syntax) for parsing timestamps. Default: `02/Jan/2006:15:04:05 -0700`. |
| **log_format_regex** | string | Optional. A Go-compatible regex to parse log lines. **Required capture groups:** `IP`, `Timestamp`. **Optional groups:** `Method`, `Path`, `StatusCode`, `Referrer`, `UserAgent`. If an optional group is omitted, its value will be treated as empty. If not provided, the application defaults to a regex that expects a **virtual-host-prefixed combined log format**. |
| **default_block_duration** | string | Optional. A global block duration to apply to any `block` action chain that does not define its own `block_duration`. Format: Go duration string (e.g., "5m", "1h"). |
| **haproxy_max_retries** | int | Optional. Number of attempts to send a command to an HAProxy instance. Default: `3`. |
| **haproxy_addresses** | array of string | A list of all HAProxy control endpoints (TCP `host:port` or Unix socket paths) across the cluster. |
| **haproxy_retry_delay** | string | Optional. Duration to wait between retry attempts. Default: `200ms`. |
| **haproxy_dial_timeout** | string | Optional. Timeout for establishing a connection to an HAProxy socket. Default: `5s`. |

#### Default Log Format Example

If `log_format_regex` is not specified, the application expects lines to follow this format:

`vhost ip - user [timestamp] "method path protocol" status size "referrer" "user-agent"`

Example:
`www.example.com 192.168.1.1 - - [02/Jan/2006:15:04:05 -0700] "GET /path HTTP/1.1" 200 1234 "http://referrer.com" "MyBrowser/1.0"`

## **BehavioralChain Definition (Top Level)**

Each item in the chains array must conform to the following structure:

| Field | Type | Required | Description |
| :---- | :---- | :---- | :---- |
| **name** | string | Yes | A unique, descriptive name for the chain (e.g., API-Abuse-Low-Agent). |
| **steps** | array of object | Yes | The sequential list of steps that define the malicious pattern. |
| **action** | string | Yes | The action to take when the chain is successfully completed by an IP. **Must be one of:** `block` or `log`. |
| **block_duration** | string | No | The duration for which the IP should be blocked if action is block. Format: Go duration string (e.g., "5m", "1h", "30m", "1h30m"). |
| **match_key** | string | Yes | The key used to track activity. This determines if behavior is tracked per IP address, per IP version, or per unique client (IP + User-Agent). See the table below for all possible values. |

#### `match_key` Values

| `match_key` | Tracks By | Description |
| :--- | :--- | :--- |
| `ip` | IP Address (v4 or v6) | Tracks activity based on the client's IP address, regardless of whether it's IPv4 or IPv6. |
| `ipv4` | IPv4 Address Only | Tracks activity based on the client's IP address. This chain will only process log entries with a valid IPv4 address. |
| `ipv6` | IPv6 Address Only | Tracks activity based on the client's IP address. This chain will only process log entries with a valid IPv6 address. |
| `ip_ua` | IP (v4/v6) + User-Agent | Tracks activity based on the combination of the client's IP address (v4 or v6) and their User-Agent string. This is useful for distinguishing different bots or clients behind the same NAT. |
| `ipv4_ua` | IPv4 + User-Agent | Tracks activity based on the combination of the client's IPv4 address and their User-Agent string. Ignores IPv6 entries. |
| `ipv6_ua` | IPv6 + User-Agent | Tracks activity based on the combination of the client's IPv6 address and their User-Agent string. Ignores IPv4 entries. |

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
| **Referrer** | `string` | The full HTTP Referer header value. |
| **UserAgent** | `string` | The HTTP User-Agent header value. |

### **Advanced `field_matches` Syntax**

The `field_matches` block supports a flexible syntax for defining match conditions, making your rules both powerful and easy to read.

#### **Simple Values (Shorthand)**

The simplest match is a direct value. The parser intelligently determines the match type.

*   **Exact String Match (Default for strings):**
    ```yaml
    Method: "POST"
    ```
*   **Exact Integer Match (Default for numbers):**
    ```yaml
    StatusCode: 404
    ```

#### **Prefixed String Matchers**

For more complex string matching, use a prefix.

*   **Exact String (Explicit):** Use `exact:` to force a literal string match for a value that could be misinterpreted as another prefix type. This is useful for rare edge cases.
    ```yaml
    Path: "exact:file:not-a-real-path" # Matches the literal string "file:not-a-real-path"
    ```

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
    The `X` acts as a wildcard for any digit.
    ```yaml
    StatusCode: "4XX" # Matches 400-499
    StatusCode: "30X" # Matches 300-309
    ```

#### **List of Values (OR Condition)**

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

#### **Object for Numeric Ranges (AND Condition)**

Use an object to define numeric ranges. This is especially useful for `StatusCode`. All conditions in the object must be met.

*   `gt`: greater than
*   `gte`: greater than or equal to
*   `lt`: less than
*   `lte`: less than or equal to
*   `not`: negates the condition

```yaml
field_matches:
  # Matches any status code from 401 to 499 (inclusive)
  StatusCode:
    gte: 401
    lt: 500

  # The 'not' operator can be used with any field type.
  # It can negate a single value or a list of values.
  Path:
    not:
      - "/admin"
      - "regex:^/api/v1/public/"
```


## **Memory Management and State Cleanup**

The bot-detector holds the state of IPs in memory. To prevent memory from growing indefinitely, two cleanup mechanisms are in place:

1.  **Idle Timeout (`idle_timeout`):** An IP's state is purged if it has been inactive (no requests seen) for longer than this duration. This is the general-purpose cleanup for all IPs, configured in the YAML file.

2.  **`min_time_since_last_hit` Optimization:** If your configuration uses `min_time_since_last_hit` rules, the application performs a more aggressive cleanup. It calculates the longest `min_time_since_last_hit` duration across all your chains. If an IP's last request is older than this duration, and it's not in the middle of a chain, its state is purged immediately, even if it hasn't reached the main `idle-timeout`. This ensures that memory is not wasted on IPs that can no longer trigger a time-based rule. If no chains use this rule, this optimization is disabled.



## **Example chains.yaml**

This example showcases a variety of features, including different matchers, time-based conditions, and actions.

For this example to be valid, you would also need a `bad_agents.txt` file in the same directory with content like:
```
# Contents of bad_agents.txt
BadBot/1.0
regex:(?i)evil-crawler
```

```yaml
version: "1.0"
default_block_duration: "30m" # Used by chains without a specific block_duration

chains:
  # --- CHAIN 1: Aggressive Scraper ---
  # Blocks an IP+UserAgent that makes 3 quick GET requests for forbidden content.
  # This uses a specific block_duration.
  - name: Aggressive-Scraper
    action: block
    block_duration: "1h"
    match_key: "ip_ua" # Track by IP and User-Agent combination
    steps:
      - field_matches:
          Method: "GET"
          StatusCode: 403 # Exact integer match
      - max_delay: "2s" # Must happen within 2s of the previous step
        min_delay: "200ms" # And must wait at least 200ms
        field_matches:
          Method: "GET"
          StatusCode: 403
      - max_delay: "2s"
        field_matches:
          Method: "GET"
          StatusCode: 403

  # --- CHAIN 2: "Sleepy" Bad Bot ---
  # Detects a bot that probes a sensitive endpoint after a long period of inactivity.
  # This uses the global default_block_duration.
  - name: Sleepy-Bot-Probe
    action: block # No block_duration, so it uses the 30m default
    match_key: "ip"
    steps:
      - min_time_since_last_hit: "20m" # Step only matches if IP was quiet for 20+ minutes
        field_matches:
          UserAgent: "file:./bad_agents.txt" # Match against a list of bad user agents
          Path: "/wp-login.php"

  # --- CHAIN 3: Multi-faceted Login Abuse (Log Only) ---
  # Logs attempts to access various login/reset paths that result in a client error.
  # This demonstrates complex `field_matches` with lists, ranges, and `ip_ua`.
  - name: Login-Abuse-Scanner
    action: "log"
    match_key: "ip_ua" # Track by IP+UA to see if a specific client is scanning.
    steps:
      - field_matches:
          # Match multiple methods (OR condition)
          Method: ["POST", "PUT"]
          # Match multiple paths (OR condition with mixed string/regex)
          Path:
            - "/api/v2/login"
            - "regex:^/reset-password/\\w+$"
          # Match any 4xx status code except 404 (AND condition)
          StatusCode:
            gte: 400
            lt: 500
            not: 404 # Note: 'not' is a powerful addition

  # --- CHAIN 4: Broken Referrer Link ---
  # A simple chain to log any IP that gets three 5xx errors in a row
  # while coming from a specific internal referrer, which might indicate a broken link.
  - name: Server-Error-Trigger
    action: "log"
    match_key: "ip"
    steps:
      - field_matches:
          StatusCode: "5XX"
          Referrer: "https://internal.my-app.com/dashboard"
      - max_delay: "10s"
        field_matches:
          StatusCode: "5XX"
          Referrer: "https://internal.my-app.com/dashboard"
      - max_delay: "10s"
        # This step uses the inline {} notation for completeness,
        # showing it's useful for simple, single-line matchers.
        field_matches: { StatusCode: "5XX", Referrer: "https://internal.my-app.com/dashboard" }
```
