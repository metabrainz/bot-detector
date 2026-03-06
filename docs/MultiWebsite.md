# Multi-Website Mode

Bot-detector can monitor multiple websites with separate log files in a single instance, with support for both global and website-specific detection rules.

## Quick Start

### Configuration

Add a `websites` section to your `config.yaml`:

```yaml
version: "1.0"

# Optional: Base directory for relative log paths
# If not specified, defaults to the config file's directory
root_dir: "/var/log/haproxy"

websites:
  - name: "main"
    vhosts: ["www.example.com", "example.com"]
    log_path: "main.log"  # Relative to root_dir → /var/log/haproxy/main.log
  
  - name: "api"
    vhosts: ["api.example.com"]
    log_path: "api/access.log"  # Relative to root_dir → /var/log/haproxy/api/access.log
  
  - name: "special"
    vhosts: ["special.example.com"]
    log_path: "/custom/location/special.log"  # Absolute path, not affected by root_dir

chains:
  # Global chain - applies to ALL websites
  - name: "Global-Scanner"
    action: "block"
    block_duration: "1h"
    match_key: "ip"
    steps: [...]
  
  # Website-specific chain
  - name: "API-Rate-Limit"
    action: "block"
    block_duration: "15m"
    match_key: "ip"
    websites: ["api"]  # Only applies to api website
    steps: [...]
  
  # Shared chain - applies to multiple websites
  - name: "SQL-Injection"
    action: "block"
    block_duration: "2h"
    match_key: "ip"
    websites: ["main", "api"]  # Applies to both
    steps: [...]
```

### Log Path Resolution

**`root_dir` (optional):** Base directory for relative log paths.

- **Not specified:** Defaults to current working directory (consistent with `--log-path` behavior)
- **Relative path:** Resolved relative to working directory (e.g., `root_dir: logs` → `/path/to/workdir/logs`)
- **Absolute path:** Used as-is (e.g., `root_dir: /var/log/haproxy`)

**`log_path` behavior:**

- **Relative path:** Resolved relative to `root_dir` (e.g., `main.log` → `/var/log/haproxy/main.log`)
- **Absolute path:** Used as-is, ignores `root_dir` (e.g., `/custom/path/log.log`)

**Startup logs show resolved paths:**
```
[INFO] MULTI_TAIL: Starting tailer for website 'main' on main.log (resolved to /var/log/haproxy/main.log)
```

### Running

```bash
# Multi-website mode (no --log-path flag needed)
./bot-detector --config-dir /etc/bot-detector

# The application detects multi-website mode automatically
# when the 'websites' section is present in config.yaml
```

## Features

### Dynamic Configuration

**No restart required** for website configuration changes:

- ✅ **Add websites** - New tailers start automatically on config reload
- ✅ **Remove websites** - Tailers stop gracefully on config reload
- ✅ **Change log paths** - Tailers restart automatically
- ✅ **Update vhosts** - Mappings update immediately
- ✅ **Modify chains** - Chain-to-website mappings update immediately

Simply edit `config.yaml` and the changes take effect on the next config reload (file watcher or SIGHUP).

### Chain Filtering

Chains can be:

- **Global** (no `websites` field) - Apply to all websites
- **Website-specific** (`websites: ["main"]`) - Apply to one website
- **Shared** (`websites: ["main", "api"]`) - Apply to multiple websites

### Website Context in Logs

Chain completion logs show which website triggered the chain:

```
[ALERT] BLOCK! Chain: API-Rate-Limit completed by IP 10.0.0.1 on website 'api' (vhost: api.example.com). Blocking for 15m
```

This makes it easy to find the triggering log line in the correct log file.

### Concurrent Processing

Each website's log is processed in its own goroutine:
- Independent log rotation handling
- Shared signal handling for coordinated shutdown
- Efficient resource usage

## Migration from Single-Website Mode

### Before (Single-Website)

```yaml
version: "1.0"
# No websites section

chains:
  - name: "Scanner"
    action: "block"
    steps: [...]
```

```bash
./bot-detector --log-path /var/log/haproxy/access.log --config-dir /etc/bot-detector
```

### After (Multi-Website)

```yaml
version: "1.0"

websites:
  - name: "main"
    vhosts: ["www.example.com"]
    log_path: "/var/log/haproxy/access.log"

chains:
  - name: "Scanner"
    action: "block"
    steps: [...]
```

```bash
./bot-detector --config-dir /etc/bot-detector
```

**Note:** The `--log-path` flag is ignored in multi-website mode. Log paths are defined in `config.yaml`.

## Cluster Deployment

Multi-website mode works with cluster configurations:

- **Leader node:** Processes logs for all websites
- **Follower nodes:** Sync configuration from leader (including website definitions)
- **Config updates:** Propagate to all nodes automatically

See [ClusterConfiguration.md](ClusterConfiguration.md) for details.

## Limitations

### Backward Compatibility

- Empty `websites` section = legacy single-website mode
- All existing single-website configurations work without modification
- `--log-path` flag still works in legacy mode

### VHost Requirement

Multi-website mode requires log entries to include a vhost field (first field in log format). This is standard in HAProxy logs:

```
www.example.com 10.0.0.1 - - [01/Jan/2026:12:00:00 +0000] "GET /test HTTP/1.1" 200 100 "-" "Bot"
```

If a log entry has an unknown vhost, it's logged once and skipped.

## Troubleshooting

### Unknown VHost Warnings

```
[WARN] Unknown vhost 'unknown.example.com' in log entry, skipping
```

**Solution:** Add the vhost to a website's `vhosts` list in `config.yaml`.

### Website Not Processing Logs

Check that:
1. Log file path is correct and readable
2. VHost in log entries matches a configured vhost
3. Log format includes vhost as first field

### Config Reload Not Updating Websites

Verify config reload is working:
```bash
# Send SIGHUP to reload
kill -HUP <pid>

# Or use file watcher (default)
# Edit config.yaml and check logs for reload message
```

## Performance

Multi-website mode has minimal overhead:
- Each website runs in its own goroutine
- Shared state is protected by efficient RWMutex
- Test results: 7.7M lines/sec throughput across 3 websites

## See Also

- [Configuration.md](Configuration.md) - Full configuration reference
- [ClusterConfiguration.md](ClusterConfiguration.md) - Cluster setup with multi-website
- [HaproxySetup.md](HaproxySetup.md) - HAProxy configuration for blocking
