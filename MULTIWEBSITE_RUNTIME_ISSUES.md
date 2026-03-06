# Multi-Website Runtime Configuration Issues

## Issues Discovered

### 1. Config Reload Doesn't Update Website Configuration
**Problem:** When config is reloaded, the following fields are NOT updated:
- `p.Websites`
- `p.VHostToWebsite`
- `p.WebsiteChains`
- `p.GlobalChains`

**Impact:** 
- Adding/removing/modifying websites requires a restart
- VHost changes won't be recognized
- Chain-to-website mappings become stale

**Location:** `internal/app/configmanager.go` - `ReloadConfig()` function

### 2. No Website Context in Chain Completion Logs
**Problem:** When a chain completes, the log shows:
```
BLOCK! Chain: API-Rate-Limit completed by IP 10.0.0.1. Blocking for 15m
```

But doesn't show WHICH website triggered it.

**Impact:**
- Hard to find the triggering log line in the correct log file
- Difficult to debug website-specific chains
- No way to correlate blocks with specific websites

**Location:** `internal/checker/checker.go` - `handleChainCompletion()` function

### 3. Multi-Website Mode Can't Be Changed at Runtime
**Problem:** Switching from single-website to multi-website mode (or vice versa) requires:
- Stopping the application
- Changing config
- Restarting

**Impact:**
- No graceful migration path
- Downtime required for mode changes

**Location:** `cmd/bot-detector/main.go` - mode is determined at startup only

## Proposed Solutions

### Solution 1: Update Website Configuration on Reload

Add to `ReloadConfig()` in `internal/app/configmanager.go`:

```go
// Update website configuration
p.Websites = loadedCfg.Websites
if len(p.Websites) > 0 {
    p.VHostToWebsite = BuildVHostMap(p.Websites)
    p.WebsiteChains, p.GlobalChains = CategorizeChains(p.Chains)
    p.LogFunc(logging.LevelInfo, "CONFIG", "Updated multi-website configuration: %d websites, %d global chains",
        len(p.Websites), len(p.GlobalChains))
} else {
    p.VHostToWebsite = nil
    p.WebsiteChains = nil
    p.GlobalChains = nil
}
```

**Limitation:** This won't restart/stop tailers for added/removed websites. Multi-website mode requires restart.

### Solution 2: Add Website Context to Chain Completion Logs

Modify `handleChainCompletion()` to include website name:

```go
// Determine website from vhost
websiteName := ""
if len(p.Websites) > 0 {
    if ws, ok := p.VHostToWebsite[entry.VHost]; ok {
        websiteName = ws
    }
}

// Log with website context
if websiteName != "" {
    p.LogFunc(logLevel, "ALERT", "BLOCK! Chain: %s completed by IP %s on website '%s' (vhost: %s). Blocking for %v%s",
        chain.Name, entry.IPInfo.Address, websiteName, entry.VHost, chain.BlockDuration, getOnMatchSuffix(chain))
} else {
    p.LogFunc(logLevel, "ALERT", "BLOCK! Chain: %s completed by IP %s. Blocking for %v%s",
        chain.Name, entry.IPInfo.Address, chain.BlockDuration, getOnMatchSuffix(chain))
}
```

### Solution 3: Document Limitation

Add to documentation that multi-website mode changes require restart:

```markdown
## Configuration Reload Limitations

### Multi-Website Mode
- **Adding/removing websites:** Requires application restart
- **Modifying website vhosts:** Requires application restart  
- **Modifying website log paths:** Requires application restart
- **Modifying chain-to-website mappings:** Updated on reload (no restart needed)

### Rationale
Multi-website mode spawns separate goroutines for each website's log tailer.
Dynamically starting/stopping these tailers during runtime would add significant
complexity and potential race conditions.

### Workaround
Use `--exit-on-eof` flag for testing website configuration changes without
affecting production.
```

## Recommendation

Implement Solutions 1 and 2 immediately (low risk, high value).
Solution 3 is documentation-only (accept the limitation).

Dynamic tailer management (full solution 3) should be deferred as it requires:
- Tailer lifecycle management
- Graceful tailer shutdown
- State cleanup for removed websites
- Significant testing
