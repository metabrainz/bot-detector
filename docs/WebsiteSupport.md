# Website Support in Bot-Detector

This document describes how website context is integrated throughout the bot-detector application, particularly in multi-website deployments.

## Overview

Bot-detector supports monitoring multiple websites with separate log files, each with global and website-specific detection rules. Website context is preserved throughout the application to provide clear visibility into which website triggered which detection.

## Website Context Format

Website context is appended to chain names using these formats:

- **Website mode**: `ChainName[website_name]`
- **VHost mode**: `ChainName@vhost.example.com`
- **Global chains**: `ChainName` (no suffix)

This format is applied consistently across:
- Block reasons
- Step identifiers
- Chain progress tracking
- Metrics and statistics
- Cluster aggregation

## Implementation Details

### 1. Chain Progress Tracking

**Location**: `internal/checker/checker.go`

Chain progress is tracked per-actor using a map where keys include website context:

```go
// Key format: "ChainName[website]" or "ChainName@vhost" or "ChainName"
chainKey := formatBlockReason(chain.Name, entry)
state, exists := currentActivity.ChainProgress[chainKey]
```

**Benefits**:
- Same IP progressing through chains on different websites is tracked separately
- No collision between website-specific chains with similar names
- Clear visibility in IP lookups showing which website each chain belongs to

### 2. Step Execution Metrics

**Location**: `internal/checker/checker.go`

Step identifiers include website context:

```go
stepIdentifier := fmt.Sprintf("step %d/%d of %s", step.Order, len(chain.Steps), formatBlockReason(chain.Name, entry))
```

**Output example**:
```
step 1/3 of Login-Abuse[main_site]
step 2/5 of API-Rate-Limit[api_site]
```

### 3. Block Reasons

**Location**: `internal/checker/checker.go`

Block reasons stored in `SkipInfo.Source` include website context:

```go
internedReason := p.InternReason(formatBlockReason(chain.Name, entry))
```

This ensures blocked IPs show which website triggered the block.

### 4. Metrics Reports

**Location**: `internal/app/providers.go`, `internal/app/metrics.go`

#### Main Stats Report (`/stats`)
- Per-chain metrics show website assignment: `ChainName [website1, website2]`
- Active chains filtered and grouped

#### Steps Report (`/stats/steps`)
- Grouped by website
- Shows global chains first, then per-website sections
- Each step includes website context in name

#### Website Stats Report (`/stats/websites`)
- Per-website metrics (lines parsed, chains matched, completions, resets)
- Unknown vhosts tracking
- Website configuration details

### 5. Cluster Endpoints

**Location**: `internal/server/handlers_cluster.go`, `internal/cluster/aggregator.go`

#### `/cluster/metrics`
Returns node metrics including:
```json
{
  "website_metrics": {
    "main_site": {
      "lines_parsed": 1000,
      "chains_matched": 50,
      "chains_reset": 2,
      "chains_completed": 48
    }
  }
}
```

#### `/cluster/metrics/aggregate`
Aggregates website metrics across all cluster nodes:
- Sums per-website statistics from all nodes
- Preserves website context in chain names
- Shows cluster-wide website activity

### 6. IP Lookup Endpoints

**Location**: `internal/server/handlers_ip.go`

#### `GET /ip/{ip}` and `GET /api/v1/ip/{ip}`

Shows chain progress with website context:

**Plain text output**:
```
status: blocked
chains:
  - Login-Abuse[main_site] (until: 2026-03-10T12:00:00Z)
  - API-Rate-Limit[api_site] (until: 2026-03-10T11:30:00Z)
```

**JSON output**:
```json
{
  "status": "blocked",
  "chains": {
    "Login-Abuse[main_site]": "2026-03-10T12:00:00Z",
    "API-Rate-Limit[api_site]": "2026-03-10T11:30:00Z"
  }
}
```

**Cluster mode**: Leader aggregates chain progress from all nodes, preserving website context.

## Configuration

### Multi-Website Configuration

```yaml
websites:
  - name: "main_site"
    vhosts: ["www.example.com", "example.com"]
    log_path: "/var/log/haproxy/main.log"
  
  - name: "api_site"
    vhosts: ["api.example.com"]
    log_path: "/var/log/haproxy/api.log"

chains:
  # Global chain - applies to all websites
  - name: "Global-Scanner"
    action: "block"
    match_key: "ip"
    steps: [...]
  
  # Website-specific chain
  - name: "Login-Abuse"
    action: "block"
    match_key: "ip_ua"
    websites: ["main_site"]  # Only applies to main_site
    steps: [...]
```

### Chain Filtering

Chains are filtered by website during processing:
- **Global chains** (no `websites` field): Apply to all websites
- **Website-specific chains**: Only apply to specified websites

See `internal/app/website.go` for chain categorization logic.

## Testing

Website context is tested in:
- `internal/checker/website_context_test.go` - Chain completion with website context
- `internal/checker/multiwebsite_global_chains_test.go` - Global vs website-specific chains
- `internal/processor/multi_website_full_integration_test.go` - End-to-end multi-website processing

## Migration Notes

### From Single-Website to Multi-Website

When migrating from single-website to multi-website mode:

1. **Chain progress**: Existing chain progress keys will not have website context. They will be treated as separate from new website-specific progress.
2. **Metrics**: Historical metrics without website context remain valid.
3. **Block reasons**: New blocks will include website context; existing blocks may not.

### Backward Compatibility

- Single-website mode continues to work without changes
- Chain names without website context are still valid
- Tests without Website/VHost fields pass unchanged

## Performance Considerations

- **Memory**: Chain progress keys are slightly longer with website context (negligible impact)
- **Lookups**: Map lookups use full key including website context (no performance impact)
- **Interning**: Block reasons are interned to reduce memory usage

## Future Enhancements

Potential improvements:
- Allow same chain name on different websites (currently rejected by config validation)
- Per-website good actors (currently all good actors are global)
- Website-specific actor cleanup policies
