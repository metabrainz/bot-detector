# Matcher Caching Optimization

> **Status:** Planning Phase
> **Last Updated:** 2025-11-18

## Executive Summary

Implement a global matcher cache to eliminate redundant field matcher evaluations across log entries. Current profiling shows matchers are evaluated hundreds of times per second with significant repetition (same protocols, methods, status codes). A usage-based cache can achieve 70-80% hit rate, providing 2-3x speedup for matcher evaluation with minimal memory overhead (~100KB).

---

## Problem Statement

### Current Behavior

Each log entry is parsed and evaluated against all behavioral chains sequentially. During evaluation, the same `field_matches` conditions are evaluated multiple times:

**Example:**
```yaml
chains:
  - name: "Chain-A"
    steps:
      - field_matches:
          method: "POST"
          statuscode: 403
      - field_matches:
          method: "POST"
          statuscode: 404

  - name: "Chain-B"
    steps:
      - field_matches:
          method: "POST"
          path: "/admin"
```

For a log entry with `method: "POST"`:
- The `method: "POST"` matcher is evaluated **3 times**
- Each evaluation performs the same string comparison
- For expensive matchers (regex, file, CIDR), redundancy is costly

### Performance Impact

**Workload**: 500 lines/sec, 10 chains, avg 3 steps per chain

**Current**: ~15,000 matcher evaluations/sec (500 × 10 × 3)

**With Cache**: ~4,500 unique evaluations + ~10,500 cache hits (**70% hit rate**)

**Most Expensive Matchers**:
1. `file:` - Multiple regex/exact matches per file line (1000+ comparisons)
2. `regex:` - Pattern compilation + matching (100-1000 CPU cycles)
3. `cidr:` - IP parsing + range check (50-100 CPU cycles)
4. `list:` - Multiple comparisons (N × comparison cost)
5. `exact:` - Single string comparison (2-5 CPU cycles) **← cheap, low priority**

---

## Solution: Global Usage-Based Matcher Cache

### Design Principles

1. **Global Scope**: Single cache shared across all log entries
2. **Usage-Based Eviction**: Keep hot matchers, evict cold ones
3. **Self-Tuning**: Adapts to actual log patterns
4. **Configurable Size**: YAML-configurable max entries
5. **FileDependency Integration**: Leverage existing checksum tracking

---

## Current Implementation Analysis

### Existing Infrastructure

**FileDependency** (`internal/types/types.go:35`):
```go
type FileDependency struct {
    Path           string
    PreviousStatus *FileDependencyStatus  // ModTime + Checksum
    CurrentStatus  *FileDependencyStatus  // ModTime + Checksum
    Content        []string               // Cached file lines
}
```
✅ **Perfect for cache invalidation!** Checksum changes = invalidate file: matchers

**Matchers** (`internal/config/config.go:285`):
```go
type fieldMatcher func(entry *app.LogEntry) bool
```
- Compiled closures, not structs
- Created at config load in `compileSingleMatcher()`
- Stored in `StepDef.Matchers[]`

**Evaluation** (`internal/checker/checker.go:394`):
```go
func matchStepFields(p *app.Processor, chain *config.BehavioralChain, step *config.StepDef, entry *app.LogEntry) bool {
    for _, matcher := range step.Matchers {
        if !matcher.Matcher(entry) {  // ← Direct closure call
            return false
        }
    }
    return true
}
```

**LogEntry Fields** (`internal/types/logentry.go:34`):
- IP, Method, Path, Protocol, Referrer, StatusCode, Size, UserAgent, VHost

---

## Cache Key Design

### Structure

```go
type CacheKey struct {
    Field       string  // "path", "statuscode", "method", etc.
    MatcherType string  // "exact", "regex", "cidr", "file", "list", "range"
    Value       string  // Canonical representation of the pattern
}

func (k CacheKey) String() string {
    return k.Field + ":" + k.MatcherType + ":" + k.Value
}
```

### Key Generation Rules

| Matcher in Config | Cache Key | Notes |
|-------------------|-----------|-------|
| `method: "GET"` | `method:exact:GET` | Simple, short |
| `statuscode: 404` | `statuscode:exact:404` | Integer as string |
| `protocol: "HTTP/1.1"` | `protocol:exact:HTTP/1.1` | Only 3 values (excellent cache hit rate!) |
| `path: "regex:^/api/.*"` | `path:regex:^/api/.*` | Pattern, not log value |
| `ip: "cidr:192.168.0.0/16"` | `ip:cidr:192.168.0.0/16` | CIDR string |
| `path: "file:./paths.txt"` | `path:file:./paths.txt:${checksum}` | **Use FileDependency.CurrentStatus.Checksum** |
| `statuscode: [401,403,404]` | `statuscode:list:401,403,404` | Sorted for consistency |
| `statuscode: {gte:400,lt:500}` | `statuscode:range:gte=400,lt=500` | Deterministic ordering |
| `path: {not: "/admin"}` | `path:negation:exact:/admin` | Includes negation type |

### Why Not Hash Long Strings?

**Cache keys are matcher patterns, not log values:**
```go
CacheKey: "path:exact:/api/v1/users/profile"  // ← Pattern (short, ~30 bytes)
LogValue: "/api/v1/users/12345/profile"       // ← Actual value from log
```

**Performance comparison** (short strings < 100 bytes):
- Direct string comparison: ~2-5ns
- Hash computation (SHA256): ~20-30ns
- **Hashing is slower for short keys**

**Exception**: `file:` matchers can be long (1000 lines), but we use FileDependency checksum instead.

**Decision**: Use string keys directly, no hashing.

---

## Cache Implementation

### Data Structures

```go
// internal/cache/matcher_cache.go
package cache

import (
    "sort"
    "strings"
    "sync"
    "time"
)

type MatcherCache struct {
    entries            map[string]*CacheEntry
    maxSize            int
    evictionPercentage int
    mu                 sync.RWMutex  // For thread-safety

    // Metrics
    totalRequests uint64
    hits          uint64
    misses        uint64
    evictions     uint64

    // Per-field metrics
    fieldStats    map[string]*FieldStats
}

type CacheEntry struct {
    Result      bool       // Match result (true/false)
    UsageCount  uint64     // How many times used
    LastUsed    time.Time  // For tie-breaking in eviction
}

type FieldStats struct {
    Hits      uint64
    Misses    uint64
    Evictions uint64
}

func NewMatcherCache(maxSize int, evictionPercentage int) *MatcherCache {
    return &MatcherCache{
        entries:            make(map[string]*CacheEntry),
        maxSize:            maxSize,
        evictionPercentage: evictionPercentage,
        fieldStats:         make(map[string]*FieldStats),
    }
}

func (c *MatcherCache) Get(key string) (result bool, found bool) {
    c.mu.RLock()
    defer c.mu.RUnlock()

    if entry, ok := c.entries[key]; ok {
        entry.UsageCount++
        entry.LastUsed = time.Now()
        c.hits++
        c.totalRequests++

        // Update per-field stats
        field := extractField(key)  // Extract "path", "statuscode", etc.
        if stats, ok := c.fieldStats[field]; ok {
            stats.Hits++
        } else {
            c.fieldStats[field] = &FieldStats{Hits: 1}
        }

        return entry.Result, true
    }

    c.misses++
    c.totalRequests++

    field := extractField(key)
    if stats, ok := c.fieldStats[field]; ok {
        stats.Misses++
    } else {
        c.fieldStats[field] = &FieldStats{Misses: 1}
    }

    return false, false
}

func (c *MatcherCache) Set(key string, result bool) {
    c.mu.Lock()
    defer c.mu.Unlock()

    // Check if we need to evict
    if len(c.entries) >= c.maxSize {
        c.evictLRU()
    }

    c.entries[key] = &CacheEntry{
        Result:     result,
        UsageCount: 1,
        LastUsed:   time.Now(),
    }
}

func (c *MatcherCache) evictLRU() {
    // Sort entries by usage count (ascending)
    type entry struct {
        key   string
        count uint64
    }

    entries := make([]entry, 0, len(c.entries))
    for k, v := range c.entries {
        entries = append(entries, entry{k, v.UsageCount})
    }

    sort.Slice(entries, func(i, j int) bool {
        return entries[i].count < entries[j].count
    })

    // Evict bottom N% (configurable)
    evictCount := (len(entries) * c.evictionPercentage) / 100
    if evictCount < 1 {
        evictCount = 1  // Always evict at least one
    }

    for i := 0; i < evictCount; i++ {
        field := extractField(entries[i].key)
        if stats, ok := c.fieldStats[field]; ok {
            stats.Evictions++
        }
        c.evictions++
        delete(c.entries, entries[i].key)
    }
}

func (c *MatcherCache) InvalidateFileMatchers(filePath string) {
    c.mu.Lock()
    defer c.mu.Unlock()

    prefix := "file:" + filePath + ":"
    for key := range c.entries {
        if strings.Contains(key, prefix) {
            delete(c.entries, key)
        }
    }
}

func (c *MatcherCache) Clear() {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.entries = make(map[string]*CacheEntry)
}

// Helper to extract field name from cache key
func extractField(key string) string {
    parts := strings.SplitN(key, ":", 2)
    if len(parts) > 0 {
        return parts[0]
    }
    return "unknown"
}

func (c *MatcherCache) GetStats() CacheStats {
    c.mu.RLock()
    defer c.mu.RUnlock()

    hitRate := 0.0
    if c.totalRequests > 0 {
        hitRate = float64(c.hits) / float64(c.totalRequests) * 100
    }

    // Deep copy field stats
    fieldStatsCopy := make(map[string]FieldStatsExport)
    for field, stats := range c.fieldStats {
        total := stats.Hits + stats.Misses
        fieldHitRate := 0.0
        if total > 0 {
            fieldHitRate = float64(stats.Hits) / float64(total) * 100
        }

        fieldStatsCopy[field] = FieldStatsExport{
            Hits:      stats.Hits,
            Misses:    stats.Misses,
            Evictions: stats.Evictions,
            HitRate:   fieldHitRate,
        }
    }

    return CacheStats{
        Size:          len(c.entries),
        MaxSize:       c.maxSize,
        Hits:          c.hits,
        Misses:        c.misses,
        HitRate:       hitRate,
        Evictions:     c.evictions,
        TotalRequests: c.totalRequests,
        FieldStats:    fieldStatsCopy,
    }
}

// Exported stats structures
type CacheStats struct {
    Size          int                         `json:"size"`
    MaxSize       int                         `json:"max_size"`
    Hits          uint64                      `json:"hits"`
    Misses        uint64                      `json:"misses"`
    HitRate       float64                     `json:"hit_rate"`
    Evictions     uint64                      `json:"evictions"`
    TotalRequests uint64                      `json:"total_requests"`
    FieldStats    map[string]FieldStatsExport `json:"field_stats"`
}

type FieldStatsExport struct {
    Hits      uint64  `json:"hits"`
    Misses    uint64  `json:"misses"`
    Evictions uint64  `json:"evictions"`
    HitRate   float64 `json:"hit_rate"`
}
```

---

## Configuration

### YAML Structure

Add to `checker` section in `config.yaml`:

```yaml
checker:
  actor_cleanup_interval: "1m"
  actor_state_idle_timeout: "30m"
  unblock_on_good_actor: true
  unblock_cooldown: "5m"

  # New cache configuration
  cache:
    enabled: true              # Enable/disable cache
    max_size: 1000             # Maximum cache entries
    eviction_percentage: 10    # Percentage to evict when full (1-50)
```

### Config Types

```go
// internal/config/types.go
type CheckerConfigYAML struct {
    UnblockOnGoodActor    bool                `yaml:"unblock_on_good_actor"`
    UnblockCooldown       string              `yaml:"unblock_cooldown"`
    ActorCleanupInterval  string              `yaml:"actor_cleanup_interval"`
    ActorStateIdleTimeout string              `yaml:"actor_state_idle_timeout"`
    Cache                 CacheConfigYAML     `yaml:"cache"`  // New
}

type CacheConfigYAML struct {
    Enabled            *bool `yaml:"enabled"`              // Pointer for optional
    MaxSize            int   `yaml:"max_size"`
    EvictionPercentage int   `yaml:"eviction_percentage"`
}

type CheckerConfig struct {
    UnblockOnGoodActor    bool
    UnblockCooldown       time.Duration
    ActorCleanupInterval  time.Duration
    ActorStateIdleTimeout time.Duration
    MaxTimeSinceLastHit   time.Duration
    Cache                 CacheConfig  // New
}

type CacheConfig struct {
    Enabled            bool
    MaxSize            int
    EvictionPercentage int
}
```

### Defaults

```go
// internal/config/config.go
const (
    DefaultCacheEnabled            = true
    DefaultCacheMaxSize            = 1000
    DefaultCacheEvictionPercentage = 10
)
```

### Validation

```go
// internal/config/config.go
func validateCacheConfig(cache *CacheConfigYAML) error {
    if cache.EvictionPercentage < 1 || cache.EvictionPercentage > 50 {
        return fmt.Errorf("cache.eviction_percentage must be between 1 and 50, got %d", cache.EvictionPercentage)
    }
    if cache.MaxSize < 10 {
        return fmt.Errorf("cache.max_size must be at least 10, got %d", cache.MaxSize)
    }
    return nil
}
```

---

## Integration Points

### 1. Matcher Compilation (internal/config/config.go)

Modify `StepDef` in `internal/config/types.go` to store cache keys alongside matchers:

```go
type StepDef struct {
    Order    int
    Matchers []struct {
        Matcher   FieldMatcher
        FieldName string
        CacheKey  string  // New: Pre-computed cache key
    }
    MaxDelayDuration    time.Duration
    MinDelayDuration    time.Duration
    MinTimeSinceLastHit time.Duration
}
```

Modify `compileSingleMatcher()` in `internal/config/config.go` to generate cache keys:

```go
func compileSingleMatcher(ctx MatcherContext, field string, value interface{}) (FieldMatcher, string, string, error) {
    // Returns: (matcher, fieldName, cacheKey, error)
    var matchers []struct {
        Matcher   fieldMatcher
        FieldName string
        CacheKey  string
    }

    ctx := MatcherContext{
        ChainName:        chainName,
        StepIndex:        stepIndex,
        FileDependencies: fileDeps,
        FilePath:         filePath,
    }

    for field, value := range fieldMatches {
        matcher, fieldName, cacheKey, err := compileSingleMatcherWithKey(ctx, field, value)
        if err != nil {
            return nil, err
        }
        matchers = append(matchers, struct {
            Matcher   fieldMatcher
            FieldName string
            CacheKey  string
        }{Matcher: matcher, FieldName: fieldName, CacheKey: cacheKey})
    }
    return matchers, nil
}
```

Add cache key generation to each matcher type in `internal/config/config.go`:

```go
// For exact matchers
func compileStringMatcher(ctx MatcherContext, value string) (FieldMatcher, string, error) {
    if strings.HasPrefix(value, "exact:") {
        literalValue := strings.TrimPrefix(value, "exact:")
        cacheKey := ctx.CanonicalFieldName + ":exact:" + literalValue

        matcher := func(entry *app.LogEntry) bool {
            fieldVal := GetMatchValueIfType(ctx.CanonicalFieldName, entry, StringField)
            if fieldVal == nil {
                return false
            }
            return strings.TrimSpace(fieldVal.(string)) == literalValue
        }
        return matcher, cacheKey, nil
    }

    // For file matchers (use checksum)
    if strings.HasPrefix(value, "file:") {
        relPath := strings.TrimPrefix(value, "file:")
        absoluteFilePath := filepath.Join(filepath.Dir(ctx.FilePath), relPath)

        fileDep := ctx.FileDependencies[absoluteFilePath]
        checksum := fileDep.CurrentStatus.Checksum

        cacheKey := ctx.CanonicalFieldName + ":file:" + absoluteFilePath + ":" + checksum

        // ... rest of file matcher compilation
        return matcher, cacheKey, nil
    }

    // For regex matchers
    if strings.HasPrefix(value, "regex:") {
        pattern := strings.TrimPrefix(value, "regex:")
        cacheKey := ctx.CanonicalFieldName + ":regex:" + pattern

        re, err := regexp.Compile(pattern)
        if err != nil {
            return nil, "", err
        }

        matcher := func(entry *app.LogEntry) bool {
            fieldVal := GetMatchValueIfType(ctx.CanonicalFieldName, entry, StringField)
            if fieldVal == nil {
                return false
            }
            return re.MatchString(fieldVal.(string))
        }
        return matcher, cacheKey, nil
    }

    // Similar for cidr, list, range, etc.
}
```

### 2. Step Evaluation (internal/checker/checker.go)

Modify `matchStepFields()` to use cache:

```go
func matchStepFields(p *app.Processor, chain *config.BehavioralChain, step *config.StepDef, entry *app.LogEntry) bool {
    cache := p.MatcherCache  // Add cache to Processor struct

    for _, m := range step.Matchers {
        // Try cache first (if enabled)
        if cache != nil {
            if result, found := cache.Get(m.CacheKey); found {
                if !result {
                    return false
                }
                continue
            }
        }

        // Cache miss - evaluate matcher
        result := m.Matcher(entry)

        // Store in cache
        if cache != nil {
            cache.Set(m.CacheKey, result)
        }

        if !result {
            return false
        }
    }
    return true
}
```

### 3. Processor Initialization (cmd/bot-detector/main.go)

Add cache to Processor in `internal/app/types.go`:

```go
type Processor struct {
    // ... existing fields
    MatcherCache *cache.MatcherCache  // New
}

func NewProcessor(config *config.AppConfig) *app.Processor {
    var matcherCache *cache.MatcherCache
    if config.Checker.Cache.Enabled {
        matcherCache = cache.NewMatcherCache(
            config.Checker.Cache.MaxSize,
            config.Checker.Cache.EvictionPercentage,
        )
    }

    return &Processor{
        // ... existing initialization
        MatcherCache: matcherCache,
    }
}
```

### 4. Config Reload (internal/app/configmanager.go)

On hot-reload, selectively invalidate cache:

```go
func (p *app.Processor) ReloadConfig(newConfig *config.AppConfig) error {
    // Check which FileDependencies changed
    for path, newDep := range newConfig.FileDependencies {
        oldDep := p.Config.FileDependencies[path]

        if oldDep == nil || oldDep.CurrentStatus.Checksum != newDep.CurrentStatus.Checksum {
            // File changed - invalidate related cache entries
            if p.MatcherCache != nil {
                p.MatcherCache.InvalidateFileMatchers(path)
            }
        }
    }

    // If config structure changed significantly, clear entire cache
    if configStructureChanged(p.Config, newConfig) {
        if p.MatcherCache != nil {
            p.MatcherCache.Clear()
        }
    }

    p.Config = newConfig
    return nil
}
```

### 5. API Endpoint Integration

Add new API endpoint for cache metrics:

```go
// internal/server/stats.go (or similar)

// Handler for GET /api/v1/metrics/cache
func (s *StatsServer) HandleCacheMetrics(w http.ResponseWriter, r *http.Request) {
    if s.processor.MatcherCache == nil {
        // Cache is disabled
        http.Error(w, `{"error": "cache is disabled"}`, http.StatusNotFound)
        return
    }

    stats := s.processor.MatcherCache.GetStats()

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(stats)
}
```

Register the endpoint:

```go
// Register in stats server initialization
mux.HandleFunc("/api/v1/metrics/cache", s.HandleCacheMetrics)
```

**Example response from `/api/v1/metrics/cache`:**

```json
{
  "size": 847,
  "max_size": 1000,
  "hits": 105432,
  "misses": 39821,
  "hit_rate": 72.6,
  "evictions": 123,
  "total_requests": 145253,
  "field_stats": {
    "method": {
      "hits": 47821,
      "misses": 2341,
      "evictions": 5,
      "hit_rate": 95.3
    },
    "protocol": {
      "hits": 48932,
      "misses": 1221,
      "evictions": 1,
      "hit_rate": 97.6
    },
    "statuscode": {
      "hits": 42134,
      "misses": 8234,
      "evictions": 23,
      "hit_rate": 83.6
    },
    "path": {
      "hits": 15234,
      "misses": 23421,
      "evictions": 87,
      "hit_rate": 39.4
    }
  }
}
```

**API Design:**

```
/api/v1/metrics/cache          → Cache-specific metrics (JSON)
/api/v1/metrics/chains         → Per-chain metrics (future)
/api/v1/metrics/actors         → Actor state metrics (future)
/api/v1/metrics/blockers       → Blocker queue/command metrics (future)

/stats                         → Legacy/simple stats endpoint (unchanged)
/metrics                       → Reserved for Prometheus format (future)
```

---

## Performance Analysis

### Expected Hit Rates (500 lines/sec workload)

Based on log analysis, field value distribution:

| Field | Unique Values | Cache Hit Rate | Notes |
|-------|---------------|----------------|-------|
| **Protocol** | ~3 (HTTP/1.0, 1.1, 2.0) | ⭐⭐⭐⭐⭐ 99%+ | Extremely repetitive |
| **Method** | ~10 (GET, POST, etc.) | ⭐⭐⭐⭐⭐ 95%+ | Highly repetitive |
| **StatusCode** | ~50-100 | ⭐⭐⭐⭐ 85-90% | Moderately repetitive |
| **UserAgent** | ~100-1000 | ⭐⭐⭐ 60-70% | Some diversity |
| **Path** | High diversity | ⭐⭐ 30-40% | Highly diverse |
| **Referrer** | High diversity | ⭐⭐ 30-40% | Highly diverse |
| **IP** | Very high | ⭐ 10-20% | Extremely diverse |

**Weighted Average Hit Rate: 70-75%**

### Performance Improvement

**Before Cache:**
- 500 entries/sec × 10 chains × 3 steps = **15,000 matcher calls/sec**
- Each call executes closure directly

**With Cache (70% hit rate):**
- 15,000 × 30% = **4,500 unique evaluations/sec**
- 15,000 × 70% = **10,500 cache hits/sec**
- Cache lookup ~5-10ns, matcher execution ~20-1000ns

**Speedup: 2-3x for matcher evaluation**

### Memory Usage

**Cache Entry Size:**
```go
type CacheEntry struct {
    Result     bool       // 1 byte
    UsageCount uint64     // 8 bytes
    LastUsed   time.Time  // 24 bytes (3 × int64)
}
// Total: ~33 bytes per entry
```

**Cache Key Size:** ~30-50 bytes (average string)

**Total per entry:** ~80-100 bytes (including map overhead)

**Max Memory:**
- 1000 entries × 100 bytes = **100 KB**
- 10,000 entries × 100 bytes = **1 MB**

**Verdict:** Negligible compared to actor state (5-10 MB for 10K active IPs)

---

## Implementation Phases

### Phase 1: Foundation (Week 1)

**Goal:** Add cache infrastructure without breaking changes

1. Create `internal/cache/matcher_cache.go`
2. Add `CacheConfig` to `types.go` and `config.go`
3. Add cache key generation to matcher compilation
4. Store cache keys in `StepDef.Matchers[]`
5. Add `MatcherCache` to `Processor` struct (nil by default)

**Testing:**
- Unit tests for cache operations (Get, Set, Evict)
- Config parsing tests for `checker.cache` section
- Verify backward compatibility (cache disabled)

**Deliverable:** Cache infrastructure ready, disabled by default

### Phase 2: Integration (Week 2)

**Goal:** Enable cache in evaluation flow

1. Modify `matchStepFields()` to use cache
2. Initialize cache in `NewProcessor()` if enabled
3. Add cache invalidation on config reload
4. Implement FileDependency checksum-based invalidation

**Testing:**
- Integration tests with cache enabled
- Verify correct evaluation results (cache vs no-cache)
- Test config reload scenarios
- Test file dependency changes

**Deliverable:** Functional caching, can be enabled via config

### Phase 3: Metrics and Tuning (Week 3)

**Goal:** Observability and optimization

1. Add cache stats to existing Metrics struct
2. Expose via `/stats` endpoint
3. Implement per-field statistics
4. Benchmark different cache sizes
5. Document optimal configuration

**Testing:**
- Performance benchmarks (500 lines/sec workload)
- Memory profiling
- Cache hit rate measurement
- Eviction behavior validation

**Deliverable:** Production-ready with full observability

---

## Testing Strategy

### Unit Tests

```go
func TestCacheHitMiss(t *testing.T)
func TestCacheEviction(t *testing.T)
func TestCacheInvalidation(t *testing.T)
func TestCacheKeyGeneration(t *testing.T)
func TestCacheFileDependencyChecksum(t *testing.T)
func TestCacheThreadSafety(t *testing.T)
```

### Integration Tests

```go
func TestCacheCorrectness(t *testing.T) {
    // Verify cached results match direct evaluation
}

func TestCacheWithConfigReload(t *testing.T) {
    // Verify cache invalidation on reload
}

func TestCacheWithFileChanges(t *testing.T) {
    // Verify file: matchers invalidate correctly
}
```

### Benchmarks

```go
func BenchmarkWithoutCache(b *testing.B) {
    // 500 entries/sec, 10 chains, 3 steps
}

func BenchmarkWithCache(b *testing.B) {
    // Same workload, cache enabled
}

// Target: >2x speedup
```

---

## Success Criteria

### Performance

- ✅ **Hit rate**: 70-80% overall (measured via `/api/v1/metrics/cache`)
- ✅ **Speedup**: 2-3x for matcher evaluation (verified by benchmarks)
- ✅ **Memory**: < 1 MB per 10K cache entries
- ✅ **Latency**: No regression in p99 log processing latency

### Correctness

- ✅ **Zero behavior changes**: Cached results match direct evaluation
- ✅ **Config reload**: Proper invalidation on file changes
- ✅ **FileDependency integration**: Checksum-based invalidation works

### Observability

- ✅ **API endpoint**: `/api/v1/metrics/cache` returns JSON cache statistics
- ✅ **Field-level breakdown**: Shows which fields benefit most from caching
- ✅ **Documented**: README section on cache configuration and API

---

## Future Enhancements

1. **Prometheus `/metrics` endpoint**: When implemented, cache stats can be exposed in Prometheus format
2. **Adaptive cache size**: Auto-tune based on hit rate
3. **TTL-based expiration**: Expire entries after N seconds
4. **Warmup on startup**: Pre-populate cache with common patterns
5. **Cache persistence**: Save/restore cache across restarts (optional)
6. **Per-chain cache statistics**: Show which chains benefit most

---

## References

- Current matcher implementation: `internal/config/config.go:285` (compileSingleMatcher and related functions)
- Step evaluation: `internal/checker/checker.go:394` (matchStepFields)
- FileDependency: `internal/types/types.go:35`
- LogEntry: `internal/types/logentry.go:34`
- Processor: `internal/app/types.go:53`
- Config types: `internal/config/types.go` (StepDef:171, CheckerConfig:57, BehavioralChain:183)
- Log processing flow: `cmd/bot-detector/main.go` → `execute()`
- API endpoints:
  - `/api/v1/metrics/cache` (new - cache statistics)
  - `/stats` (existing - general stats)
  - `/metrics` (reserved - future Prometheus format)

---

## Appendix: Cache Key Examples

### All Matcher Types

```go
// Exact string
"path:exact:/admin"

// Regex
"path:regex:^/api/v[12]/"

// CIDR
"ip:cidr:192.168.0.0/16"

// File (with checksum)
"path:file:./paths.txt:a1b2c3d4e5f6..."

// List (sorted)
"statuscode:list:401,403,404"

// Range
"statuscode:range:gte=400,lt=500"

// Negation
"path:negation:exact:/admin"

// Negation with list
"method:negation:list:DELETE,PUT"

// Complex object
"statuscode:range:gte=400,lt=500,not=404"
```
