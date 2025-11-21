# v2 Format Optimization Roadmap

This document tracks planned optimizations for a future v2 persistence format.

## Current Status (v1 Format)

### Implemented Optimizations ✅

| Optimization | Savings (500k IPs) | Status | Commit |
|--------------|-------------------|--------|--------|
| Pre-allocate slice capacity | Negligible | ✅ Done | 1ec7720 |
| gzip.BestSpeed compression | ~3x faster writes | ✅ Done | 932a60a |
| Compact JSON (no indent) | ~10-15% smaller files | ✅ Done | c100cb0 |
| In-place filtering | ~50% less memory | ✅ Done | cc3820c |
| BlockState uint8 | 7 MB | ✅ Done | 8e184eb |
| String interning (reasons) | 17 MB | ✅ Done | 1c43948 |

**Total savings: ~24 MB (26% reduction) + faster compaction**

### Memory Footprint (v1)

Per blocked IP entry:
- Map key (IP string): 16 bytes (header) + 7-45 bytes (data) = ~27 bytes
- ActiveBlockInfo: 40 bytes
- IPState: 56 bytes (with uint8 BlockState)
- Reason string: 16 bytes (header) + data (interned, shared)
- Map overhead: ~16 bytes

**Total: ~170 bytes per blocked IP (after optimizations)**
**500k IPs: ~81 MB**

---

## Planned Optimizations for v2

### Phase 1: Remove ActiveBlocks Redundancy ⭐ HIGH PRIORITY

**Status**: Planned for v1 → v2 migration

**Problem**: We maintain two maps with overlapping data
```go
ActiveBlocks map[string]ActiveBlockInfo  // 40 bytes per blocked IP
IPStates     map[string]IPState          // 56 bytes per blocked IP
```

**Solution**: Use only `IPStates`, derive `ActiveBlocks` when needed

**Savings**: 56 bytes per IP = **27 MB for 500k IPs**

**Migration Path**:
1. ✅ v1 already has IPStates as source of truth
2. ⏸️ Update all code to read from IPStates only
3. ⏸️ v1 snapshots stop writing ActiveBlocks
4. ⏸️ v0 compatibility: Load ActiveBlocks → populate IPStates
5. ⏸️ After all production files migrated to v1: Remove ActiveBlocks entirely

**Complexity**: Medium (requires code refactoring)
**Breaking**: Yes (for v0 format support)
**Timeline**: After v1 is stable in production

---

### Phase 2: Binary IP Representation 🔄 MEDIUM PRIORITY

**Status**: Research phase

**Problem**: IPs stored as strings consume significant memory
```go
// Current
map[string]IPState  // Key: "192.168.1.1" (27 bytes) or "2001:..." (55 bytes)

// Proposed
map[[16]byte]IPState  // Key: Fixed 16 bytes for all IPs
```

**Savings**: 
- IPv4: 27 → 16 bytes = 11 bytes per IP
- IPv6: 55 → 16 bytes = 39 bytes per IP
- Map key not duplicated in value
- **Total: ~14 MB for 500k IPs (90% IPv4, 10% IPv6)**

**Implementation Options**:

#### Option A: Fixed 16-byte Array (Simplest)
```go
type IP [16]byte

func ParseIP(s string) IP {
    ip := net.ParseIP(s)
    var result IP
    copy(result[:], ip.To16())  // IPv4 becomes IPv4-mapped IPv6
    return result
}

func (ip IP) String() string {
    return net.IP(ip[:]).String()
}

// Usage
IPStates map[IP]IPState
```

**Pros**: Simple, fixed size, fast comparisons
**Cons**: IPv4 uses 16 bytes (12 bytes wasted)

#### Option B: Variable Length (More Complex)
```go
type IP struct {
    data [16]byte
    len  uint8  // 4 for IPv4, 16 for IPv6
}
```

**Pros**: IPv4 only uses 5 bytes
**Cons**: More complex, harder to use as map key

#### Option C: Separate Maps (Hybrid)
```go
IPv4States map[[4]byte]IPState
IPv6States map[[16]byte]IPState
```

**Pros**: Optimal memory for each type
**Cons**: Duplicate code, complexity

**Recommendation**: Option A (fixed 16 bytes)
- Simplest implementation
- Still saves 11 bytes per IPv4 (40% reduction)
- Saves 39 bytes per IPv6 (70% reduction)
- Fast, predictable performance

**Changes Required**:
- Update all IP string handling to binary
- Custom JSON marshaling/unmarshaling
- Update logging to convert to string
- Migration: Parse all string IPs to binary on load

**Complexity**: High (touches entire codebase)
**Breaking**: Yes (file format change)
**Timeline**: v2 format only

---

### Phase 3: Compact Timestamp Representation 🔄 LOW PRIORITY

**Status**: Deferred

**Problem**: `time.Time` is 24 bytes (wall clock + monotonic + location)

**Solution**: Use `int64` Unix timestamp (8 bytes)

**Savings**: 16 bytes per IP = **8 MB for 500k IPs**

**Trade-offs**:
- ✅ Saves memory
- ✅ Faster comparisons
- ❌ Lose nanosecond precision (only need second precision)
- ❌ Lose timezone info (not needed, always UTC)
- ❌ Less convenient to work with

**Decision**: Keep `time.Time` for now
- Future-proof (nanosecond precision may be useful)
- Convenience outweighs 8 MB savings
- Can revisit if memory becomes critical

---

### Phase 4: Reason ID Table 🔄 LOW PRIORITY

**Status**: Deferred (string interning is sufficient)

**Problem**: Even with interning, reason strings have 16-byte headers

**Solution**: Use uint16 reason IDs with lookup table
```go
type ReasonID uint16

var ReasonTable = map[ReasonID]string{
    1: "Immediate-Language-Setter-Bot",
    2: "API-Abuse-Chain",
    // ...
}

type IPState struct {
    State      BlockState
    ExpireTime time.Time
    ReasonID   ReasonID  // 2 bytes instead of 16-byte string header
}
```

**Savings**: 14 bytes per IP = **7 MB for 500k IPs**

**Complexity**: 
- Need to manage ID assignment
- Need to persist table across restarts
- Need to handle ID exhaustion (uint16 = 65k reasons)
- Migration complexity

**Decision**: Not worth it
- String interning already saves most memory
- Added complexity not justified by 7 MB savings
- String interning is simpler and more maintainable

---

## Summary: v2 Format Goals

### Target Memory Footprint

| Component | v1 (current) | v2 (planned) | Savings |
|-----------|--------------|--------------|---------|
| Map key (IP) | 27 bytes | 16 bytes | 11 bytes |
| ActiveBlockInfo | 40 bytes | 0 bytes (removed) | 40 bytes |
| IPState | 56 bytes | 56 bytes | 0 bytes |
| Reason | 16 bytes (interned) | 16 bytes | 0 bytes |
| Map overhead | 16 bytes | 16 bytes | 0 bytes |
| **Total** | **~170 bytes** | **~104 bytes** | **~66 bytes (39%)** |

**For 500k IPs:**
- v1: ~81 MB
- v2: ~49 MB
- **Savings: 32 MB (40% reduction)**

### Implementation Timeline

**Phase 1: Remove ActiveBlocks** (v1.x → v2.0)
- Prerequisite: All production files migrated to v1
- Effort: Medium (2-3 days)
- Savings: 27 MB
- Risk: Medium (requires careful testing)

**Phase 2: Binary IPs** (v2.0 → v2.1)
- Prerequisite: Phase 1 complete
- Effort: High (1-2 weeks)
- Savings: 14 MB
- Risk: High (touches entire codebase)

**Total v2 Savings: 41 MB (51% reduction from v1)**

---

## Migration Strategy: v1 → v2

### Step 1: Deprecate ActiveBlocks (v1.5)
- Add `GetActiveBlocks()` helper
- Update all code to read from IPStates
- Keep ActiveBlocks populated for compatibility
- Mark as deprecated in comments

### Step 2: v2 Format Without ActiveBlocks (v2.0)
- v2 snapshots don't write ActiveBlocks
- v2 snapshots don't populate ActiveBlocks on load
- v1 snapshots still supported (populate IPStates from ActiveBlocks)
- v0 snapshots still supported (populate IPStates from ActiveBlocks)

### Step 3: Binary IPs (v2.1)
- Add IP type as `[16]byte`
- Add conversion functions
- Update all IP handling
- v2.1 snapshots use binary IPs
- v2.0/v1/v0 snapshots still load (convert strings to binary)

### Step 4: Drop v0/v1 Support (v3.0)
- Remove v0 format support
- Remove v1 format support
- Remove ActiveBlocks field entirely
- Clean up all compatibility code

---

## Testing Requirements

### Memory Profiling
```bash
# Before optimization
go test -memprofile=mem_before.prof -bench=BenchmarkCompaction

# After optimization
go test -memprofile=mem_after.prof -bench=BenchmarkCompaction

# Compare
go tool pprof -base=mem_before.prof mem_after.prof
```

### Load Testing
- Generate 500k IP test data
- Measure memory usage during:
  - Snapshot load
  - Journal replay
  - Compaction
  - Steady state
- Verify savings match predictions

### Compatibility Testing
- v0 → v1 migration
- v1 → v2 migration
- Mixed version cluster (if applicable)
- Rollback scenarios

---

## Open Questions

1. **Binary IP Format**: Should we use IPv4-mapped IPv6 or separate types?
   - Recommendation: IPv4-mapped (simpler)

2. **ActiveBlocks Removal**: Can we remove it in v1.x or wait for v2?
   - Recommendation: Wait for v2 (breaking change)

3. **Backward Compatibility**: How long to support v0/v1 after v2 release?
   - Recommendation: 2 major versions (v2 supports v1/v0, v3 drops them)

4. **Migration Tool**: Should we provide a standalone migration tool?
   - Recommendation: Yes, for large deployments

---

## References

- Optimization audit: [Commit 03efe9b]
- BlockState uint8: [Commit 8e184eb]
- String interning: [Commit 1c43948]
- In-place filtering: [Commit cc3820c]

---

## Changelog

- 2025-11-21: Initial roadmap created
- 2025-11-21: BlockState uint8 implemented (7 MB saved)
- 2025-11-21: String interning implemented (17 MB saved)
