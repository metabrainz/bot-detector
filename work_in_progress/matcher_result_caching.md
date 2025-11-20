# Matcher Result Caching - Level 2 Optimization

## Overview

This document describes the implementation plan for caching matcher evaluation results to avoid redundant matcher executions when the same LogEntry is evaluated against multiple chains with duplicate matchers.

## Current State (Level 1)

**Field Value Caching** (✅ Implemented):
- Caches extracted field values (e.g., `Path` → "/login")
- Avoids redundant `GetMatchValue()` calls
- Performance: ~4.9x faster for field extraction
- Location: `internal/types/logentry.go`

## Problem Statement

When a LogEntry is processed through multiple chains:
1. Field values are extracted once (cached) ✅
2. Matcher evaluation happens repeatedly (NOT cached) ❌

**Example:**
```yaml
chains:
  - name: "Chain1"
    steps:
      - field_matches:
          path: "regex:^/login"        # Evaluates regex
  - name: "Chain2"
    steps:
      - field_matches:
          path: "regex:^/login"        # Evaluates SAME regex AGAIN!
```

**Current execution:**
- Chain 1: Extract path (cache miss), evaluate regex (500ns)
- Chain 2: Extract path (cache HIT!), evaluate regex AGAIN (500ns)
- **Waste: 500ns per duplicate matcher**

## Solution: Matcher Result Caching (Level 2)

Cache the boolean result of `matcher.Matcher(entry)` using a cache key that uniquely identifies the matcher configuration.

### Architecture

```
┌─────────────┐
│  LogEntry   │
├─────────────┤
│ fieldCache  │  Level 1: Field value extraction cache
│             │  Key: "Path:0" → "/login"
├─────────────┤
│matcherCache │  Level 2: Matcher result cache (NEW)
│             │  Key: "path:regex:^/login" → true/false
└─────────────┘
```

## Implementation Plan

### Phase 1: Data Structures

#### 1.1 Add matcherCache to LogEntry

**File**: `internal/types/logentry.go`

```go
type LogEntry struct {
    Timestamp  time.Time
    IPInfo     utils.IPInfo
    // ... other fields ...

    // Level 1: Field value cache
    fieldCache map[string]interface{}

    // Level 2: Matcher result cache
    matcherCache map[string]bool
}
```

**Export for access from matchers:**
```go
// GetMatcherCache returns the matcher result cache (creates if needed)
func (e *LogEntry) GetMatcherCache() map[string]bool {
    if e.matcherCache == nil {
        e.matcherCache = make(map[string]bool)
    }
    return e.matcherCache
}

// CheckMatcherCache looks up a cached matcher result
func (e *LogEntry) CheckMatcherCache(cacheKey string) (bool, bool) {
    if e.matcherCache == nil {
        return false, false
    }
    result, found := e.matcherCache[cacheKey]
    return result, found
}

// StoreMatcherResult caches a matcher evaluation result
func (e *LogEntry) StoreMatcherResult(cacheKey string, result bool) {
    if e.matcherCache == nil {
        e.matcherCache = make(map[string]bool)
    }
    e.matcherCache[cacheKey] = result
}
```

### Phase 2: Cache Key Generation

#### 2.1 Cache Key Format Specification

Each matcher type generates a unique, deterministic cache key:

| Matcher Type | Cache Key Format | Example |
|--------------|------------------|---------|
| **exact** | `field:exact:value` | `path:exact:/login` |
| **regex** | `field:regex:pattern` | `path:regex:^/api/` |
| **cidr** | `field:cidr:network` | `ip:cidr:192.168.0.0/16` |
| **file** | `field:file:path:checksum` | `ip:file:/etc/ips.txt:a1b2c3` |
| **list** | `field:list:val1,val2,...` (sorted) | `method:list:DELETE,GET,POST` |
| **range** | `field:range:op=val,op=val` | `statuscode:range:gte=400,lt=500` |
| **not** | `field:not:subcachekey` | `path:not:regex:^/admin` |
| **statuscode pattern** | `field:pattern:prefix` | `statuscode:pattern:4` |

**Key Requirements:**
1. **Deterministic**: Same matcher → same key
2. **Unique**: Different matchers → different keys
3. **Sortable**: List items must be sorted (e.g., `[POST, GET]` → `GET,POST`)
4. **Canonical**: Use normalized field names

#### 2.2 Helper Function for Cache Key Generation

**File**: `internal/config/config.go`

```go
// generateMatcherCacheKey creates a unique cache key for a matcher configuration.
// This key is used to cache matcher evaluation results in LogEntry.matcherCache.
func generateMatcherCacheKey(fieldName, matcherType, value string) string {
    // Format: "fieldName:matcherType:value"
    // Example: "path:regex:^/login"
    return fieldName + ":" + matcherType + ":" + value
}

// sortAndJoin sorts a slice of strings and joins them with a delimiter.
// Used for list matchers to ensure deterministic cache keys.
func sortAndJoin(items []string, delimiter string) string {
    sorted := make([]string, len(items))
    copy(sorted, items)
    sort.Strings(sorted)
    return strings.Join(sorted, delimiter)
}
```

### Phase 3: Matcher Compilation Modifications

#### 3.1 Minimal Code Change Approach

**Strategy**: Wrap matcher functions with caching logic WITHOUT modifying StepDef structure.

Instead of adding `CacheKey` field to StepDef.Matchers, we **embed the cache key inside the matcher closure**.

**Advantages:**
- ✅ No changes to StepDef structure
- ✅ No changes to matcher execution in checker.go
- ✅ Minimal code changes
- ✅ Cache key generation happens during compilation
- ✅ Caching logic is localized to each matcher type

**Pattern:**
```go
func compileSomeMatcher(ctx MatcherContext, value string) (FieldMatcher, error) {
    // 1. Generate cache key
    cacheKey := generateMatcherCacheKey(ctx.CanonicalFieldName, "matchertype", value)

    // 2. Compile matcher logic (regex, etc.)
    // ...

    // 3. Return wrapped matcher with caching
    return func(entry *types.LogEntry) bool {
        // Check cache
        if result, found := entry.CheckMatcherCache(cacheKey); found {
            return result // Cache hit
        }

        // Evaluate matcher (original logic)
        result := /* ... matcher evaluation ... */

        // Store result
        entry.StoreMatcherResult(cacheKey, result)

        return result
    }, nil
}
```

### Phase 4: Matcher-Specific Implementation

#### 4.1 Exact String Matcher

**File**: `internal/config/config.go` (CompileStringMatcher)

```go
if strings.HasPrefix(value, "exact:") {
    literalValue := strings.TrimPrefix(value, "exact:")
    cacheKey := generateMatcherCacheKey(ctx.CanonicalFieldName, "exact", literalValue)

    return func(entry *types.LogEntry) bool {
        if result, found := entry.CheckMatcherCache(cacheKey); found {
            return result
        }

        fieldVal := types.GetMatchValueIfType(ctx.CanonicalFieldName, entry, types.StringField)
        result := fieldVal != nil && fieldVal.(string) == literalValue

        entry.StoreMatcherResult(cacheKey, result)
        return result
    }, nil
}
```

#### 4.2 Regex Matcher (HIGH PRIORITY - Most Expensive)

**Cost**: ~500-2000 ns per evaluation (depending on pattern complexity)

```go
if strings.HasPrefix(value, "regex:") {
    pattern := strings.TrimPrefix(value, "regex:")
    re, err := regexp.Compile(pattern)
    if err != nil {
        return nil, fmt.Errorf("invalid regex: %w", err)
    }

    cacheKey := generateMatcherCacheKey(ctx.CanonicalFieldName, "regex", pattern)

    return func(entry *types.LogEntry) bool {
        if result, found := entry.CheckMatcherCache(cacheKey); found {
            return result
        }

        fieldVal := types.GetMatchValueIfType(ctx.CanonicalFieldName, entry, types.StringField)
        if fieldVal == nil {
            entry.StoreMatcherResult(cacheKey, false)
            return false
        }

        result := re.MatchString(fieldVal.(string))
        entry.StoreMatcherResult(cacheKey, result)
        return result
    }, nil
}
```

#### 4.3 CIDR Matcher

**Cost**: ~200-400 ns per evaluation

```go
if strings.HasPrefix(value, "cidr:") {
    cidrStr := strings.TrimPrefix(value, "cidr:")
    _, ipNet, err := net.ParseCIDR(cidrStr)
    if err != nil {
        return nil, fmt.Errorf("invalid CIDR: %w", err)
    }

    cacheKey := generateMatcherCacheKey(ctx.CanonicalFieldName, "cidr", cidrStr)

    return func(entry *types.LogEntry) bool {
        if result, found := entry.CheckMatcherCache(cacheKey); found {
            return result
        }

        fieldVal := types.GetMatchValueIfType(ctx.CanonicalFieldName, entry, types.StringField)
        if fieldVal == nil {
            entry.StoreMatcherResult(cacheKey, false)
            return false
        }

        ip := net.ParseIP(fieldVal.(string))
        result := ip != nil && ipNet.Contains(ip)
        entry.StoreMatcherResult(cacheKey, result)
        return result
    }, nil
}
```

#### 4.4 List Matcher

**Cost**: ~50-200 ns per evaluation (depends on list size)

**Challenge**: Need to sort list items for deterministic cache key.

```go
func compileListMatcher(ctx MatcherContext, values []interface{}) (FieldMatcher, error) {
    // Convert values to strings for cache key (simplified)
    var strValues []string
    for _, v := range values {
        strValues = append(strValues, fmt.Sprintf("%v", v))
    }
    cacheKey := generateMatcherCacheKey(ctx.CanonicalFieldName, "list", sortAndJoin(strValues, ","))

    // Compile sub-matchers
    var subMatchers []FieldMatcher
    for _, item := range values {
        matcher, _, err := compileSingleMatcher(ctx, ctx.CanonicalFieldName, item)
        if err != nil {
            return nil, err
        }
        subMatchers = append(subMatchers, matcher)
    }

    return func(entry *types.LogEntry) bool {
        if result, found := entry.CheckMatcherCache(cacheKey); found {
            return result
        }

        // OR logic: check if any sub-matcher matches
        for _, matcher := range subMatchers {
            if matcher(entry) {
                entry.StoreMatcherResult(cacheKey, true)
                return true
            }
        }

        entry.StoreMatcherResult(cacheKey, false)
        return false
    }, nil
}
```

**ISSUE**: This creates a **recursive caching problem**. Sub-matchers will cache their own results, so caching the list result is redundant.

**SOLUTION**: Only cache **leaf matchers** (exact, regex, cidr), NOT composite matchers (list, not, object).

#### 4.5 Range Matcher

**Cost**: ~20-50 ns per evaluation (just integer comparison)

**Decision**: **Skip caching** - overhead of cache lookup (~20ns) negates benefit.

#### 4.6 Not Matcher

**Decision**: **Skip caching** - delegates to sub-matcher which is already cached.

#### 4.7 File Matcher

**Cost**: ~100-500 ns per evaluation (delegates to list matcher)

**Challenge**: Cache key must include file checksum to invalidate on file changes.

```go
// In CompileStringMatcher, file: branch
cacheKey := generateMatcherCacheKey(
    ctx.CanonicalFieldName,
    "file",
    absoluteFilePath + ":" + fileDep.CurrentStatus.Checksum,
)
```

**Problem**: Checksum changes require cache invalidation, which is complex.

**Decision**: **Skip caching for now** - delegates to sub-matchers which are cached.

#### 4.8 Status Code Pattern Matcher (e.g., "4XX")

**Cost**: ~30-50 ns per evaluation

**Decision**: **Cache it** - simple and deterministic.

```go
// In CompileStringMatcher, status code pattern branch
if ctx.CanonicalFieldName == "StatusCode" && strings.Contains(strings.ToUpper(value), "X") {
    xIndex := strings.Index(strings.ToUpper(value), "X")
    if xIndex > 0 {
        prefix := value[:xIndex]
        if _, err := strconv.Atoi(prefix); err == nil {
            cacheKey := generateMatcherCacheKey(ctx.CanonicalFieldName, "pattern", prefix)

            return func(entry *types.LogEntry) bool {
                if result, found := entry.CheckMatcherCache(cacheKey); found {
                    return result
                }

                fieldVal := types.GetMatchValueIfType(ctx.CanonicalFieldName, entry, types.IntField)
                result := fieldVal != nil && strings.HasPrefix(strconv.Itoa(fieldVal.(int)), prefix)

                entry.StoreMatcherResult(cacheKey, result)
                return result
            }, nil
        }
    }
}
```

#### 4.9 Default String Matcher (trimmed exact match)

**Cost**: ~20-30 ns per evaluation (string comparison)

**Decision**: **Skip caching** - overhead not worth it.

### Phase 5: Caching Strategy Summary

| Matcher Type | Cache? | Reason |
|--------------|--------|--------|
| **exact** | ❌ Skip | Too cheap (~20ns), overhead not worth it |
| **regex** | ✅ **YES** | Expensive (~500-2000ns), high value |
| **cidr** | ✅ **YES** | Moderately expensive (~200-400ns) |
| **file** | ❌ Skip | Delegates to sub-matchers (already cached) |
| **list** | ❌ Skip | Delegates to sub-matchers (already cached) |
| **range** | ❌ Skip | Too cheap (~20-50ns) |
| **not** | ❌ Skip | Delegates to sub-matcher (already cached) |
| **statuscode pattern** | ✅ Maybe | Cheap but simple to cache |
| **default exact** | ❌ Skip | Too cheap (~20-30ns) |

**Priority Order:**
1. **Regex** (highest impact)
2. **CIDR** (medium impact)
3. **Status pattern** (low impact, but easy)

### Phase 6: Testing & Validation

#### 6.1 Unit Tests

**File**: `internal/types/logentry_test.go`

```go
func TestMatcherCache_Basic(t *testing.T) {
    entry := &LogEntry{Path: "/login"}

    // First call - cache miss
    entry.StoreMatcherResult("path:regex:^/login", true)

    // Second call - cache hit
    result, found := entry.CheckMatcherCache("path:regex:^/login")
    if !found || !result {
        t.Errorf("Expected cache hit with result=true")
    }
}

func TestMatcherCache_DifferentKeys(t *testing.T) {
    entry := &LogEntry{Path: "/login"}

    entry.StoreMatcherResult("path:regex:^/login", true)
    entry.StoreMatcherResult("path:regex:^/api", false)

    result1, _ := entry.CheckMatcherCache("path:regex:^/login")
    result2, _ := entry.CheckMatcherCache("path:regex:^/api")

    if !result1 || result2 {
        t.Errorf("Cache keys not isolated")
    }
}
```

#### 6.2 Integration Tests

**File**: `internal/config/config_test.go`

```go
func TestRegexMatcher_Caching(t *testing.T) {
    // Create two chains with same regex matcher
    yaml := `
version: "1.0"
chains:
  - name: "Chain1"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          path: "regex:^/login"
  - name: "Chain2"
    match_key: "ip"
    action: "log"
    steps:
      - field_matches:
          path: "regex:^/login"
`
    // Load config, process entry through both chains
    // Verify matcher is evaluated only once
}
```

#### 6.3 Benchmarks

**File**: `internal/types/logentry_benchmark_test.go`

```go
// Benchmark regex matcher without caching (baseline)
func BenchmarkRegexMatcher_NoCaching(b *testing.B) {
    entry := &LogEntry{Path: "/login"}
    re := regexp.MustCompile("^/login")

    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        _ = re.MatchString(entry.Path)
    }
}

// Benchmark regex matcher with caching (10 repeated evaluations)
func BenchmarkRegexMatcher_WithCaching(b *testing.B) {
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        entry := &LogEntry{Path: "/login"}
        cacheKey := "path:regex:^/login"

        // Simulate 10 chains checking same regex
        for chain := 0; chain < 10; chain++ {
            if result, found := entry.CheckMatcherCache(cacheKey); found {
                _ = result
            } else {
                re := regexp.MustCompile("^/login")
                result := re.MatchString(entry.Path)
                entry.StoreMatcherResult(cacheKey, result)
            }
        }
    }
}
```

### Phase 7: Performance Analysis

#### 7.1 Expected Speedup

**Scenario: 10 chains, all checking `path: "regex:^/login"`**

**Without matcher caching:**
- Chain 1: Extract path (cache miss) + Regex eval (500ns) = 650ns
- Chains 2-10: Extract path (cache hit) + Regex eval (500ns) = 520ns × 9 = 4,680ns
- **Total: 5,330ns**

**With matcher caching:**
- Chain 1: Extract path (cache miss) + Regex eval (500ns) + Store result = 650ns
- Chains 2-10: Extract path (cache hit) + Matcher cache hit (20ns) = 140ns × 9 = 1,260ns
- **Total: 1,910ns**

**Speedup: 2.8x faster** 🚀

#### 7.2 Memory Overhead

**Per LogEntry:**
- fieldCache: ~100-200 bytes (5-10 entries × ~20 bytes each)
- matcherCache: ~50-100 bytes (2-5 unique matchers × ~20 bytes each)
- **Total: ~150-300 bytes per LogEntry**

**Acceptable trade-off** for 2-3x performance improvement.

### Phase 8: Implementation Checklist

**Phase 1: Infrastructure**
- [ ] Add `matcherCache map[string]bool` to LogEntry
- [ ] Add `CheckMatcherCache()` method
- [ ] Add `StoreMatcherResult()` method
- [ ] Add helper functions for cache key generation

**Phase 2: Regex Matcher (High Priority)**
- [ ] Modify `CompileStringMatcher` regex branch
- [ ] Add cache key generation for regex patterns
- [ ] Wrap regex evaluation with caching logic
- [ ] Write unit tests for regex caching
- [ ] Benchmark regex matcher with/without caching

**Phase 3: CIDR Matcher (Medium Priority)**
- [ ] Modify `CompileStringMatcher` cidr branch
- [ ] Add cache key generation for CIDR networks
- [ ] Wrap CIDR evaluation with caching logic
- [ ] Write unit tests for CIDR caching
- [ ] Benchmark CIDR matcher

**Phase 4: Status Pattern Matcher (Low Priority)**
- [ ] Modify status code pattern branch
- [ ] Add cache key generation
- [ ] Wrap evaluation with caching logic

**Phase 5: Testing & Validation**
- [ ] Run full test suite
- [ ] Run benchmarks
- [ ] Validate no regressions
- [ ] Measure real-world performance improvement

### Phase 9: Future Optimizations

1. **Cache key interning**: Use string interning to reduce memory for duplicate cache keys
2. **Cache eviction**: Implement LRU eviction if matcherCache grows too large (unlikely)
3. **Cache statistics**: Add metrics to track cache hit rates
4. **File matcher caching**: Handle checksum-based invalidation properly

### Phase 10: Risks & Mitigations

| Risk | Mitigation |
|------|------------|
| Cache key collisions | Use deterministic, unique key generation with field name prefix |
| Memory bloat | Only cache expensive matchers (regex, CIDR) |
| Stale cache data | Cache is per-entry, discarded after processing |
| Complexity | Start with regex only, expand incrementally |
| File matcher invalidation | Skip caching file matchers for now |

## Conclusion

This Level 2 optimization provides significant performance gains (2-3x speedup) for workloads with multiple chains checking the same patterns. The implementation is incremental, starting with the highest-impact matchers (regex) and expanding as needed.

**Recommended approach:**
1. Implement infrastructure + regex matcher caching first
2. Benchmark and validate
3. Incrementally add CIDR and other matchers if needed
4. Monitor real-world performance improvements
