# Migrating to Multi-Website Mode

This guide explains how to migrate from single-website mode to multi-website mode in bot-detector.

## Overview

Bot-detector now supports monitoring multiple websites with separate log files in a single instance. This eliminates the need to run multiple bot-detector processes, simplifies configuration management, and allows for both global and website-specific detection rules.

## Key Concepts

### Single-Website Mode (Legacy)
- One bot-detector instance per website
- Log file specified via `--log-path` flag
- All chains apply to all traffic
- Separate configuration file per instance

### Multi-Website Mode (New)
- One bot-detector instance for multiple websites
- Log files specified in `config.yaml`
- Chains can be global or website-specific
- Single configuration file for all websites
- `--log-path` flag is ignored

## Migration Steps

### Step 1: Identify Your Websites

List all websites you're currently monitoring with separate bot-detector instances:

```
Instance 1: www.example.com, example.com → /var/log/haproxy/main.log
Instance 2: api.example.com → /var/log/haproxy/api.log
Instance 3: admin.example.com → /var/log/haproxy/admin.log
```

### Step 2: Update Configuration File

Add a `websites` section to your `config.yaml`:

**Before (single-website):**
```yaml
version: "1.0"

application:
  log_level: "info"

# ... other settings ...

chains:
  - name: "Scanner-Detection"
    action: "block"
    match_key: "ip"
    steps: [...]
```

**After (multi-website):**
```yaml
version: "1.0"

# NEW: Define websites
websites:
  - name: "main_site"
    vhosts: ["www.example.com", "example.com"]
    log_path: "/var/log/haproxy/main.log"
  
  - name: "api_site"
    vhosts: ["api.example.com"]
    log_path: "/var/log/haproxy/api.log"
  
  - name: "admin_site"
    vhosts: ["admin.example.com"]
    log_path: "/var/log/haproxy/admin.log"

application:
  log_level: "info"

# ... other settings ...

chains:
  # This chain now applies to ALL websites
  - name: "Scanner-Detection"
    action: "block"
    match_key: "ip"
    # No 'websites' field = global
    steps: [...]
```

### Step 3: Categorize Your Chains

Review your existing chains and decide which should be:
- **Global** (apply to all websites)
- **Website-specific** (apply to one or more websites)

**Example:**

```yaml
chains:
  # Global chains - no 'websites' field
  - name: "Global-Aggressive-Scanner"
    action: "block"
    match_key: "ip"
    steps: [...]
  
  # Website-specific chains
  - name: "Main-Login-Abuse"
    action: "block"
    match_key: "ip_ua"
    websites: ["main_site"]  # Only for main_site
    steps: [...]
  
  - name: "API-Rate-Limit"
    action: "block"
    match_key: "ip"
    websites: ["api_site"]  # Only for api_site
    steps: [...]
  
  # Shared chains - multiple websites
  - name: "SQL-Injection-Detection"
    action: "block"
    match_key: "ip"
    websites: ["main_site", "api_site"]  # Both sites
    steps: [...]
```

### Step 4: Merge Configurations

If you have separate configuration files for each website, merge them:

1. **Combine all chains** into a single `chains` section
2. **Add `websites` field** to website-specific chains
3. **Keep global chains** without `websites` field
4. **Merge `good_actors`** (they apply globally by default)
5. **Use one set of `blockers` settings** (shared across all websites)

### Step 5: Update Startup Command

**Before (single-website):**
```bash
# Instance 1
./bot-detector --log-path /var/log/haproxy/main.log --config-dir /etc/bot-detector/main

# Instance 2
./bot-detector --log-path /var/log/haproxy/api.log --config-dir /etc/bot-detector/api

# Instance 3
./bot-detector --log-path /var/log/haproxy/admin.log --config-dir /etc/bot-detector/admin
```

**After (multi-website):**
```bash
# Single instance for all websites
./bot-detector --config-dir /etc/bot-detector
```

**Note:** The `--log-path` flag is ignored in multi-website mode.

### Step 6: Update Systemd Service (if applicable)

**Before:**
```ini
# /etc/systemd/system/bot-detector-main.service
[Service]
ExecStart=/usr/local/bin/bot-detector --log-path /var/log/haproxy/main.log --config-dir /etc/bot-detector/main

# /etc/systemd/system/bot-detector-api.service
[Service]
ExecStart=/usr/local/bin/bot-detector --log-path /var/log/haproxy/api.log --config-dir /etc/bot-detector/api
```

**After:**
```ini
# /etc/systemd/system/bot-detector.service
[Service]
ExecStart=/usr/local/bin/bot-detector --config-dir /etc/bot-detector
```

Then reload and restart:
```bash
systemctl daemon-reload
systemctl stop bot-detector-main bot-detector-api bot-detector-admin
systemctl start bot-detector
systemctl enable bot-detector
```

## Validation

### 1. Check Configuration
```bash
./bot-detector --config-dir /etc/bot-detector --check
```

### 2. Test in Dry-Run Mode
```bash
# Test with one of your log files
./bot-detector --dry-run --config-dir /etc/bot-detector
```

### 3. Monitor Startup Logs
```bash
./bot-detector --config-dir /etc/bot-detector
```

Look for:
```
[SETUP] Multi-website mode: 3 websites, 5 global chains
[MULTI_TAIL] Starting tailer for website 'main_site' on /var/log/haproxy/main.log
[MULTI_TAIL] Starting tailer for website 'api_site' on /var/log/haproxy/api.log
[MULTI_TAIL] Starting tailer for website 'admin_site' on /var/log/haproxy/admin.log
```

### 4. Verify Chain Processing
Check that website-specific chains only trigger for the correct websites:
```bash
# Watch logs for chain matches
tail -f /var/log/bot-detector.log | grep -E "CHAIN_COMPLETE|BLOCK"
```

## Rollback Plan

If you need to rollback to single-website mode:

1. **Remove the `websites` section** from `config.yaml`
2. **Remove `websites` fields** from chains
3. **Restart with `--log-path` flag**:
   ```bash
   ./bot-detector --log-path /var/log/haproxy/main.log --config-dir /etc/bot-detector
   ```

## Common Issues

### Issue: "Unknown vhost" warnings

**Symptom:**
```
[UNKNOWN_VHOST] Unknown vhost 'staging.example.com' for IP 1.2.3.4 - only global chains will be processed
```

**Solution:** Add the vhost to the appropriate website's `vhosts` list:
```yaml
websites:
  - name: "main_site"
    vhosts: ["www.example.com", "example.com", "staging.example.com"]
    log_path: "/var/log/haproxy/main.log"
```

### Issue: Chain not triggering for expected website

**Symptom:** A website-specific chain isn't matching traffic.

**Solution:** Verify:
1. The `websites` field references the correct website name
2. The vhost in log entries matches the website's `vhosts` list
3. The chain is not disabled (action doesn't start with `!`)

### Issue: "--log-path is ignored" warning

**Symptom:**
```
[CONFIG] --log-path flag is ignored in multi-website mode. Log paths are defined in config.yaml
```

**Solution:** This is expected. Remove the `--log-path` flag from your startup command.

## Best Practices

### 1. Start with Global Chains
Begin by making most chains global, then gradually make them website-specific as needed:
```yaml
chains:
  # Start global
  - name: "Scanner-Detection"
    action: "block"
    match_key: "ip"
    steps: [...]
  
  # Make specific later if needed
  - name: "Scanner-Detection"
    action: "block"
    match_key: "ip"
    websites: ["main_site"]  # Now specific
    steps: [...]
```

### 2. Use Descriptive Website Names
Choose names that clearly identify the website:
```yaml
websites:
  - name: "main_site"      # Good
  - name: "api_v2"         # Good
  - name: "site1"          # Bad - unclear
```

### 3. Group Related Vhosts
Put all vhosts for a single website together:
```yaml
websites:
  - name: "main_site"
    vhosts: 
      - "www.example.com"
      - "example.com"
      - "www.example.net"  # Alternate domain
      - "example.net"
    log_path: "/var/log/haproxy/main.log"
```

### 4. Monitor Resource Usage
Multi-website mode uses more resources than single-website mode. Monitor:
- Memory usage (one tailer per website)
- CPU usage (concurrent log processing)
- File descriptor usage (one file handle per website)

### 5. Test Configuration Changes
Always test configuration changes with `--check` before reloading:
```bash
./bot-detector --config-dir /etc/bot-detector --check
```

## Example: Complete Migration

**Before - 3 separate instances:**

`/etc/bot-detector/main/config.yaml`:
```yaml
version: "1.0"
chains:
  - name: "Login-Abuse"
    action: "block"
    steps: [...]
```

`/etc/bot-detector/api/config.yaml`:
```yaml
version: "1.0"
chains:
  - name: "Rate-Limit"
    action: "block"
    steps: [...]
```

**After - 1 unified instance:**

`/etc/bot-detector/config.yaml`:
```yaml
version: "1.0"

websites:
  - name: "main_site"
    vhosts: ["www.example.com"]
    log_path: "/var/log/haproxy/main.log"
  - name: "api_site"
    vhosts: ["api.example.com"]
    log_path: "/var/log/haproxy/api.log"

chains:
  - name: "Login-Abuse"
    action: "block"
    websites: ["main_site"]
    steps: [...]
  
  - name: "Rate-Limit"
    action: "block"
    websites: ["api_site"]
    steps: [...]
```

## Support

For questions or issues:
1. Check the [Configuration.md](Configuration.md) documentation
2. Review the [example configuration](../testdata/multiwebsite_config.yaml)
3. Open an issue on GitHub with your configuration (sanitized)
