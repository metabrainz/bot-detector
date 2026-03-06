# Multi-Website Implementation Progress

## Completed Steps

### Step 1: Configuration Types and Validation ✅
**Commit:** 7390910 - "feat: add multi-website configuration types and validation"

- Added `WebsiteConfig` type with name, vhosts, and log_path fields
- Added `Websites` field to `BehavioralChain` for website-specific chains
- Added `Websites` field to `LoadedConfig`
- Implemented `validateWebsites()` with comprehensive validation
- Added 9 comprehensive tests for website validation
- All tests passing

**Files Modified:**
- `internal/config/types.go` (+30 lines)
- `internal/config/config.go` (+60 lines)
- `internal/config/website_test.go` (new file, 120 lines)

### Step 2: Processor Fields and Helpers ✅
**Commit:** 68347ce - "feat: add processor fields and helpers for multi-website support"

- Added `Websites`, `VHostToWebsite`, `WebsiteChains`, `GlobalChains` to Processor
- Implemented `BuildVHostMap()` to create vhost->website mapping
- Implemented `CategorizeChains()` to separate global vs website-specific chains
- Added comprehensive test coverage for both helper functions
- All tests passing

**Files Modified:**
- `internal/app/types.go` (+10 lines)
- `internal/app/website.go` (new file, 40 lines)
- `internal/app/website_test.go` (new file, 160 lines)

### Step 3: Website-Aware Chain Processing ✅
**Commit:** 96f68c3 - "feat: implement website-aware chain processing"

- Modified `checkChainsInternal` to filter chains based on vhost
- In multi-website mode: process global chains + website-specific chains
- In legacy mode (no websites): process all chains as before
- Log warning for unknown vhosts, only process global chains
- Added comprehensive tests for chain filtering logic
- All existing tests still passing

**Files Modified:**
- `internal/checker/checker.go` (+40 lines)
- `internal/checker/website_test.go` (new file, 130 lines)

## Remaining Steps

### Step 4: Multi-Log Tailer (Next Priority)
**Estimated:** 3-4 hours

Create `internal/processor/multi_log.go` with:
- `MultiLogTailer()` function to tail multiple log files concurrently
- One goroutine per website
- Shared signal handling for graceful shutdown
- Error handling per website

**Files to Create/Modify:**
- `internal/processor/multi_log.go` (new, ~100 lines)
- `internal/processor/multi_log_test.go` (new, ~150 lines)

### Step 5: Main Integration
**Estimated:** 2 hours

Modify `cmd/bot-detector/main.go` to:
- Detect multi-website vs legacy mode
- Initialize processor fields for multi-website
- Call `MultiLogTailer` or `LiveLogTailer` based on mode
- Maintain backward compatibility

**Files to Modify:**
- `cmd/bot-detector/main.go` (+80 lines)

### Step 6: Documentation
**Estimated:** 3-4 hours

Update documentation with:
- Multi-website configuration examples
- Migration guide from single to multi-website
- API changes (if any)
- Best practices

**Files to Modify:**
- `docs/Configuration.md` (+150 lines)
- `README.md` (+50 lines)
- Create `docs/MultiWebsite.md` (new, ~200 lines)

### Step 7: Integration Testing
**Estimated:** 4 hours

- Create end-to-end tests with multiple log files
- Test config hot-reload with websites
- Test backward compatibility
- Test error scenarios

**Files to Create:**
- `cmd/bot-detector/multiwebsite_test.go` (new, ~300 lines)
- Test fixtures in `testdata/`

## Testing Status

All tests passing:
```
✅ internal/config (11 tests)
✅ internal/app (8 tests)
✅ internal/checker (all existing + 2 new tests)
```

## Architecture Decisions Made

1. **VHost-based routing**: Use existing vhost field from logs
2. **Chain categorization**: Pre-compute global vs website-specific chains at startup
3. **Legacy compatibility**: Empty `websites` section = legacy single-website mode
4. **Unknown vhost handling**: Log warning, process only global chains
5. **Minimal code changes**: ~300 lines of new code total

## Next Session TODO

1. Implement `MultiLogTailer` in `internal/processor/multi_log.go`
2. Add tests for multi-log tailing
3. Integrate into main.go
4. Update documentation
5. Run full test suite
6. Create example configurations

## Configuration Example (for reference)

```yaml
version: "1.0"

# NEW: Multi-website support
websites:
  - name: "main"
    vhosts: ["www.example.com", "example.com"]
    log_path: "/var/log/haproxy/main.log"
  - name: "api"
    vhosts: ["api.example.com"]
    log_path: "/var/log/haproxy/api.log"

# Existing sections remain unchanged
application:
  log_level: "info"

chains:
  # Global chain - applies to ALL websites
  - name: "Global-Scanner"
    action: "block"
    match_key: "ip"
    steps: [...]
    
  # Website-specific chain
  - name: "API-Rate-Limit"
    action: "block"
    match_key: "ip"
    websites: ["api"]  # NEW: only for api website
    steps: [...]
```
