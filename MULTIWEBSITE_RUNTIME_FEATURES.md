# Multi-Website Runtime Configuration

## Features Implemented

### 1. Dynamic Website Configuration Updates ✅

**Status:** IMPLEMENTED - No restart required

Website configuration is now fully dynamic:
- **Adding websites:** New tailers start automatically on config reload
- **Removing websites:** Tailers stop gracefully on config reload  
- **Modifying website vhosts:** VHost mappings update immediately
- **Modifying website log paths:** Tailers restart automatically with new path
- **Modifying chain-to-website mappings:** Updates immediately on reload

**Implementation:** `internal/processor/multi_website_manager.go`

The `MultiWebsiteTailerManager` manages website tailers dynamically:
- Each website has its own goroutine and stop channel
- Config reload triggers `UpdateWebsites()` which:
  - Stops tailers for removed websites
  - Starts tailers for new websites
  - Restarts tailers if log path changed
  - Keeps existing tailers running if unchanged

### 2. Website Context in Chain Completion Logs ✅

**Status:** IMPLEMENTED

Chain completion logs now include website context:

**Before:**
```
BLOCK! Chain: API-Rate-Limit completed by IP 10.0.0.1. Blocking for 15m
```

**After:**
```
BLOCK! Chain: API-Rate-Limit completed by IP 10.0.0.1 on website 'api' (vhost: api.example.com). Blocking for 15m
```

**Implementation:** `internal/checker/checker.go`

Benefits:
- Easy to find triggering log line in correct log file
- Clear correlation between blocks and websites
- Better debugging for website-specific chains

## Configuration Reload Behavior

### What Updates Without Restart
- ✅ Adding/removing websites
- ✅ Modifying website vhosts
- ✅ Modifying website log paths
- ✅ Modifying chain-to-website mappings
- ✅ All other configuration (chains, good actors, etc.)

### Architecture

```
Config Reload
    ↓
ReloadConfig() in configmanager.go
    ↓
Updates p.Websites, p.VHostToWebsite, p.WebsiteChains
    ↓
Calls manager.UpdateWebsites()
    ↓
Manager compares old vs new websites
    ↓
├─ Stop removed website tailers
├─ Start new website tailers  
└─ Restart tailers with changed log paths
```

## Example Scenarios

### Adding a Website

1. Edit `config.yaml`, add new website:
```yaml
websites:
  - name: "new_site"
    vhosts: ["new.example.com"]
    log_path: "/var/log/haproxy/new.log"
```

2. Config reload triggers (file watcher or SIGHUP)

3. Log output:
```
[CONFIG] Updated multi-website tailers: 3 websites, 2 global chains
[MULTI_TAIL] Starting tailer for new website 'new_site'
[TAIL] Starting log tailer on /var/log/haproxy/new.log...
```

4. New website is immediately active - no restart needed!

### Removing a Website

1. Edit `config.yaml`, remove website

2. Config reload triggers

3. Log output:
```
[MULTI_TAIL] Stopping tailer for removed website 'old_site'
[MULTI_TAIL] Tailer stopped for website 'old_site'
[CONFIG] Updated multi-website tailers: 2 websites, 2 global chains
```

4. Tailer stops gracefully - no restart needed!

### Changing Log Path

1. Edit `config.yaml`, change log_path for existing website

2. Config reload triggers

3. Log output:
```
[MULTI_TAIL] Log path changed for website 'api', restarting tailer
[MULTI_TAIL] Tailer stopped for website 'api'
[MULTI_TAIL] Starting tailer for website 'api' on /var/log/haproxy/api_new.log
```

4. Tailer restarts with new path - no restart needed!

## Testing

Comprehensive tests in `internal/processor/multi_website_manager_test.go`:
- `TestMultiWebsiteTailerManager_DynamicAdd` - Adding websites at runtime
- `TestMultiWebsiteTailerManager_DynamicRemove` - Removing websites at runtime
- `TestMultiWebsiteTailerManager_LogPathChange` - Changing log paths at runtime

All tests pass ✅

## Design Principles Maintained

✅ **No restart required** - Core bot-detector principle maintained  
✅ **Graceful degradation** - Failed tailers don't affect others  
✅ **Thread-safe** - All operations protected by mutexes  
✅ **Observable** - All changes logged clearly  
✅ **Testable** - Comprehensive test coverage

