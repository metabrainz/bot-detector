# Behavioral Bot Detector (bot-detector)

The `bot-detector` is a high-performance Go-based utility designed to analyze web access logs in real-time, detect sophisticated, multi-step malicious behavior (bot chains), and dynamically block offending IP addresses using the HAProxy Runtime API.

The program is optimized for minimal CPU and memory overhead, utilizing pre-compiled regular expressions and a dedicated, timed cleanup routine to prevent memory leaks during long-running operation.

## ⚙️ Building the Application

To compile the source code, you must first initialize the Go module and fetch the external dependencies (specifically `gopkg.in/yaml.v3`).

1. **Initialize the Go Module:**

```

go mod init bot_detector

```

2. **Fetch Dependencies:**

```

go mod tidy

```

3. **Build the Executable:**

```

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

```

sudo ./bot-detector  
\--log-path "/var/log/nginx/access.log"  
\--socket-path "/var/run/haproxy/admin.sock"  
\--map-path "/etc/haproxy/maps/blocked\_ips.map"  
\--idle-timeout "30m"

```

### 2. Dry Run Mode

Dry Run mode is for testing your behavioral rules without connecting to HAProxy or running live cleanup routines.

| **Flag** | **Description** | **Default** | 
 | ----- | ----- | ----- | 
| `--dry-run` | Activates test mode (skips HAProxy and live tailing). | `false` | 
| `--test-log` | Path to a static log file to process lines from. | `test_access.log` | 

**Example Command (Testing Rules):**

```

./bot-detector --dry-run  
\--yaml-path "test\_rules.yaml"  
\--test-log "large\_test\_data.log"

```

## 🧩 Configuration File Example (`chains.yaml`)

The behavioral chains are defined in a YAML file, specifying a sequence of steps that must occur within specific time windows to trigger an action.

Each chain requires an **`action`** field, which determines the system's response when the chain is completed:

* **`block`**: The IP address is immediately added to the HAProxy block map for the duration specified by `block_duration`.

* **`log`**: The detection is logged to the system output, but *no* block command is sent to HAProxy. This is ideal for testing new chains in a live production environment without risking disruption.

The core fields you can match against are: **IP, Path, Method, UserAgent, Referrer, and StatusCode.**

### `chains.yaml`

```yaml

version: "1.0"
chains:

# --- CHAIN 1: The Account Takeover Pre-scan ---

# Goal: Block users who first probe for a sensitive path and then immediately

# attempt a high volume of login attempts.

  - name: "Login-Bruteforce-Attempt"
    action: "block"
    block_duration: "2h"
    steps:

    # Step 1: Probe a known sensitive file

      - order: 1
        field_matches:
        Path: "/wp-admin/includes/version.php"
        StatusCode: "404|403" # Look for failure responses

    # Step 2: Immediate follow-up with multiple login attempts

      - order: 2
        field_matches:
        Path: "/login.html"
        Method: "POST"
        max_delay: "15s" # This step must occur within 15 seconds of Step 1

# --- CHAIN 2: The Evasion Scanner ---

# Goal: Block IPs that scrape a public page followed by an administrative area,

# but use a known bot User Agent and try to appear like a normal browser.

  - name: "Evasion-Scraper"
    action: "log" # Using 'log' for testing purposes
    block_duration: "10m"
    steps:

    # Step 1: Hit a public page

      - order: 1
        field_matches:
        Path: "^/products/[a-z0-9-]+$"
        UserAgent: "(?i)Mozilla/5.0.*Chrome" # Use common browser UA regex to avoid detection
        # Note: If max_delay is omitted, it defaults to no maximum delay for the next step.

    # Step 2: Hit a private/admin endpoint

      - order: 2
        field_matches:
        Path: "/admin/stats"
        StatusCode: "401" # Check for Unauthorized access
        max_delay: "3m" # Must occur within 3 minutes of Step 1

```
