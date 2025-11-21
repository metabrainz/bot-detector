# Cluster Implementation Plan

> **Status:** Planning Phase
> **Last Updated:** 2025-11-17

This document outlines the phased implementation plan for bot-detector's cluster architecture, starting with low-hanging fruits and progressing to more complex features.

## Implementation Strategy

The plan progresses from:
- **Phases 1-3:** Foundation and read-only endpoints (no cluster interaction)
- **Phases 4-5:** Follower → Leader communication (one-way)
- **Phases 6-7:** Leader → Follower communication (complete the loop)
- **Phases 8-10:** Hardening and security
- **Phases 11-12:** Testing and polish

Each phase is independently testable and provides incremental value. Early phases can be merged and deployed without breaking existing functionality since cluster features are opt-in via the `--leader` flag.

---

## Phase 1: Foundation - Cluster Configuration and Node Identity ⭐ START HERE

**Status:** Not Started
**Complexity:** Low
**Dependencies:** None

### Why First
Pure data structure work with no behavioral changes. Provides the foundation for all other phases and can be tested in isolation.

### What to Implement
1. Add cluster config to YAML structure in `internal/config/types.go` and `internal/config/config.go`
2. Add `--leader` flag to `internal/commandline/parse.go`
3. Create `internal/cluster/types.go` with:
   - `NodeConfig` struct (name, address)
   - `ClusterConfig` struct (nodes list, intervals, protocol)
   - `NodeRole` enum (Leader, Follower)
4. Create `internal/cluster/identity.go` with:
   - Function to determine node identity from listen address
   - Function to match address against cluster.nodes

### Files to Create/Modify
- `internal/cluster/types.go` (NEW)
- `internal/cluster/identity.go` (NEW)
- `internal/config/types.go` - Add cluster config fields to AppConfig
- `internal/config/config.go` - Parse cluster section from YAML
- `internal/commandline/parse.go` - Add `Leader` field to AppParameters

### Testing
- Unit tests for YAML parsing with cluster section
- Unit tests for node identity matching
- Test with and without cluster config (backward compatibility)
- Test various cluster configurations

### Value
Enables all other phases, fully backward compatible

---

## Phase 2: Health and Status Endpoints

**Status:** Not Started
**Complexity:** Low
**Dependencies:** Phase 1

### Why Here
Simple, read-only endpoint. No cluster interaction yet. Provides immediate value for monitoring.

### What to Implement
1. Create `internal/cluster/status.go` with status response struct
2. Add status handler to `internal/server/stats.go`
3. Response includes: role, node name, uptime, version, config last modified

### Files to Create/Modify
- `internal/cluster/status.go` (NEW)
- `internal/server/stats.go` - Add `/api/v1/status` handler
- `internal/app/types.go` - Add method to Processor to get status info

### Testing
- HTTP test for status endpoint
- Verify JSON structure
- Test as leader and follower mode

### Value
Immediate monitoring capability for all nodes

---

## Phase 3: Metrics API Endpoint (Follower Side)

**Status:** Not Started
**Complexity:** Low
**Dependencies:** Phase 1

### Why Here
Still read-only, no cluster communication. Enables followers to expose their metrics for collection.

### What to Implement
1. Create `internal/cluster/metrics.go` with metrics response struct
2. Add metrics serialization to JSON (reuse existing metrics data)
3. Add handler to `internal/server/stats.go`
4. Include node name in response

### Files to Create/Modify
- `internal/cluster/metrics.go` (NEW)
- `internal/server/stats.go` - Add `/api/v1/metrics` handler
- `internal/metrics/metrics.go` - Add JSON serialization methods

### Testing
- HTTP test for metrics endpoint
- Verify JSON structure matches spec
- Test with various metric values

### Value
Enables followers to expose metrics for leader collection

---

## Phase 4: Configuration Polling (Follower Side)

**Status:** Not Started
**Complexity:** Medium
**Dependencies:** Phase 1, existing `/config/archive` endpoint

### Why Here
First actual cluster interaction, but only one-way (follower → leader). No complex coordination yet.

### What to Implement
1. Create `internal/cluster/follower.go` with:
   - `ConfigPoller` struct
   - Periodic HEAD request to check Last-Modified header
   - GET request to download archive on change
   - Trigger existing hot-reload mechanism
2. Start poller goroutine in `cmd/bot-detector/main.go` when in follower mode
3. Use `config_poll_interval` from config
4. Handle checksum verification (ETag)

### Files to Create/Modify
- `internal/cluster/follower.go` (NEW)
- `cmd/bot-detector/main.go` - Start config poller when `--leader` flag is set

### Testing
- Mock HTTP server as leader
- Verify HEAD requests occur at correct interval
- Verify GET only happens when Last-Modified changes
- Verify hot-reload is triggered
- Test checksum validation

### Value
Core config distribution mechanism

---

## Phase 5: Bootstrap Mode (Follower First-Run)

**Status:** Not Started
**Complexity:** Medium
**Dependencies:** Phase 4

### Why Here
Natural extension of Phase 4. Special case of config polling.

### What to Implement
1. Extend `internal/cluster/follower.go` with bootstrap detection:
   - Check if config file exists on startup with `--leader` flag
   - If not, download from leader's `/config/archive`
   - Extract to config directory
   - Exit with code 0 and message
2. Add bootstrap logic to `cmd/bot-detector/main.go` before normal execution

### Files to Create/Modify
- `internal/cluster/follower.go` - Add `Bootstrap()` function
- `cmd/bot-detector/main.go` - Check and run bootstrap before `execute()`

### Testing
- Test with no local config (should download and exit)
- Test with existing config (should skip bootstrap)
- Test extraction of archive contents
- Test error handling (leader unreachable)

### Value
Easy onboarding of new nodes

---

## Phase 6: Metrics Collection (Leader Side)

**Status:** ✅ Complete
**Complexity:** Medium
**Dependencies:** Phase 3

### Why Here
Completes the metrics flow. Still relatively simple (just HTTP GET requests).

### What Was Implemented
1. Created `internal/cluster/leader.go` with:
   - `MetricsCollector` struct with HTTP client, poll interval, metrics map, mutex
   - `CollectedMetrics` struct to store node metrics, timestamps, and error info
   - Periodic GET requests to all nodes' `/cluster/metrics` endpoint
   - Thread-safe storage of latest metrics from each node
   - Error tracking with consecutive failure counting
   - Graceful error handling (logs warnings, continues polling other nodes)
2. Started collector goroutine in `cmd/bot-detector/main.go` for leader nodes
3. Uses `metrics_report_interval` from cluster config (defaults to 60s)
4. Added `Cluster` field to Processor struct for access to cluster configuration
5. Fixed bug: Config poll interval now uses `config_poll_interval` instead of hardcoded 30s

### Files Created/Modified
- `internal/cluster/leader.go` (NEW) - MetricsCollector implementation
- `internal/cluster/leader_test.go` (NEW) - Comprehensive unit tests
- `internal/app/types.go` - Added Cluster field to Processor struct
- `cmd/bot-detector/main.go` - Start metrics collector for leaders, fixed config poll interval

### Testing
✅ Mock HTTP servers as followers (httptest)
✅ Verify GET requests occur at correct interval
✅ Verify metrics are stored and retrieved correctly
✅ Verify error handling (unreachable, HTTP errors, malformed JSON)
✅ Verify recovery from errors (consecutive error reset)
✅ Verify thread safety of GetCollectedMetrics()
✅ All 17 package tests pass

### Value
Enables cluster-wide metrics visibility - leader now collects metrics from all nodes

---

## Phase 7: Cluster Metrics Aggregation and Dashboard

**Status:** ✅ Complete
**Complexity:** Medium
**Dependencies:** Phase 6

### Why Here
Builds on Phase 6's data collection. Provides user-visible cluster view.

### What Was Implemented
1. Created `internal/cluster/aggregator.go` with:
   - `NodeHealthStatus` enum (healthy, stale, error)
   - `NodeMetricsInfo` struct for per-node data
   - `AggregatedMetrics` struct for cluster-wide view
   - Helper functions: `sumInt64Maps`, `sumProcessingStats`, `sumActorStats`, `sumChainStats`, `sumChainMetrics`
   - `determineNodeHealth()` to assess node status based on errors and staleness
   - `AggregateMetrics()` main function that combines all node metrics
2. Added `GetAggregatedMetrics()` to Provider interface in `internal/server/types.go`
3. Implemented `GetAggregatedMetrics()` in `internal/app/providers.go` with:
   - Access to MetricsCollector
   - Stale threshold calculation (3x poll interval)
   - Calls to `cluster.AggregateMetrics()`
4. Created `clusterMetricsAggregateHandler()` in `internal/server/handlers_cluster.go`
5. Registered `/cluster/metrics/aggregate` route in `internal/server/server.go`
6. Added `MetricsCollector` field to Processor struct
7. Stored MetricsCollector reference when starting collector in `cmd/bot-detector/main.go`

### Files Created/Modified
- `internal/cluster/aggregator.go` (NEW - 200+ lines)
- `internal/cluster/aggregator_test.go` (NEW - 9 comprehensive tests)
- `internal/server/types.go` - Added `GetAggregatedMetrics()` to Provider
- `internal/server/handlers_cluster.go` - Added aggregate handler
- `internal/server/server.go` - Registered new route, updated docs
- `internal/app/types.go` - Added MetricsCollector field
- `internal/app/providers.go` - Implemented GetAggregatedMetrics()
- `cmd/bot-detector/main.go` - Stored collector reference

### Testing
✅ Test helper functions (sumInt64Maps, sumProcessingStats, sumActorStats, sumChainStats, sumChainMetrics)
✅ Test node health determination (healthy, stale, error)
✅ Test full aggregation with all healthy nodes
✅ Test aggregation with mixed health states
✅ Test aggregation with missing/nil snapshots
✅ All 9 new tests pass
✅ All 47 cluster package tests pass

### Value
Complete cluster dashboard with aggregated metrics and per-node health status

---

## Phase 8: Error Handling and Network Resilience

**Status:** Not Started
**Complexity:** Medium
**Dependencies:** Phases 4, 6

### Why Here
Hardens all previous phases. Should be done before considering production use.

### What to Implement
1. Add timeouts to all HTTP requests (use cluster config values)
2. Add retry logic with exponential backoff
3. Comprehensive logging for cluster events
4. Graceful degradation when leader/followers unavailable
5. Add HTTP client configuration (timeouts, keep-alive, etc.)

### Files to Create/Modify
- `internal/cluster/http.go` (NEW) - Shared HTTP client with proper config
- `internal/cluster/follower.go` - Add error handling and retries
- `internal/cluster/leader.go` - Add error handling and retries
- All cluster files - Add logging statements

### Testing
- Test timeout scenarios
- Test retry behavior
- Test continued operation when cluster communication fails
- Test log output at various levels

### Value
Production-ready reliability

---

## Phase 9: Configuration Validation and Safety

**Status:** Not Started
**Complexity:** Low
**Dependencies:** Phase 1

### Why Here
Ensures cluster config is valid before operation. Prevents common mistakes.

### What to Implement
1. Validate cluster config during load in `config.go`:
   - Node addresses are valid (host:port format)
   - No duplicate node names or addresses
   - Intervals are reasonable (not too short)
   - Own address exists in nodes list (when cluster enabled)
2. Add `--check` flag support for cluster config
3. Warn if node count is 1 (single-node cluster is odd)

### Files to Create/Modify
- `internal/config/config.go` - Add cluster validation logic
- `internal/cluster/validate.go` (NEW)

### Testing
- Test with invalid node addresses
- Test with duplicate names
- Test with intervals too short
- Test with node not in cluster list

### Value
Prevents common configuration mistakes

---

## Phase 10: Protocol Selection (HTTP/HTTPS)

**Status:** Not Started
**Complexity:** Low
**Dependencies:** Phase 8

### Why Here
Security enhancement. Relatively simple once HTTP client is abstracted.

### What to Implement
1. Read `http_protocol` from cluster config
2. Update HTTP client creation in `internal/cluster/http.go` to use correct scheme
3. Add TLS configuration options (optional, can be basic for now)
4. Update all URL construction to use configured protocol

### Files to Create/Modify
- `internal/cluster/http.go` - Add protocol selection
- `internal/config/config.go` - Parse `http_protocol` field

### Testing
- Test with http:// URLs
- Test with https:// URLs (may need test certificates)
- Test fallback/default behavior

### Value
Secure cluster communication

---

## Phase 11: Integration Testing and Documentation

**Status:** Not Started
**Complexity:** Medium
**Dependencies:** All previous phases

### Why Here
Final validation before production use. Ensures everything works together.

### What to Implement
1. Create integration test that runs 3 instances (1 leader, 2 followers)
2. Test config propagation
3. Test metrics aggregation
4. Test bootstrap process
5. Test failover scenario
6. Update README.md with cluster setup instructions
7. Create deployment examples (systemd units, docker-compose)

### Files to Create/Modify
- `integration_test.go` (NEW)
- `docs/ClusterSetup.md` (NEW)
- `README.md` - Add cluster section
- `examples/cluster/` (NEW directory with examples)

### Testing
- Full end-to-end cluster operation
- Simulate various failure scenarios
- Verify all documented procedures work

### Value
Validates everything works together, provides deployment guidance

---

## Phase 12: Optional Enhancements (Post-MVP)

**Status:** Future
**Complexity:** Varies
**Dependencies:** Phase 11

### Why Last
Nice-to-have features that aren't critical for initial cluster support.

### Potential Features
1. Metrics visualization dashboard (HTML/JS)
2. Authentication/authorization for cluster endpoints
3. Prometheus-compatible cluster metrics
4. Alert system for follower failures
5. Automatic follower discovery
6. Configuration diff endpoint
7. Cluster health score calculation

### Implementation
Each feature would be its own sub-phase

### Value
Incremental improvements to cluster experience

---

## Key Implementation Principles

1. **Backward Compatibility:** All cluster features are opt-in via `--leader` flag
2. **Independent Execution:** Core threat detection continues independently of cluster state
3. **Fail-Safe:** Cluster communication failures don't affect threat detection/blocking
4. **Go Idioms:** Small packages, clear interfaces, fail-fast error handling
5. **Testability:** Each phase is independently testable
6. **Incremental Value:** Each phase can be deployed to production

## Architecture Notes

- **No automatic leader election:** Manual failover is intentional for simplicity
- **Duplicate blocks acceptable:** HAProxy handles duplicate stick-table updates
- **Pull-based metrics:** Avoids push complexity and network issues
- **Independent state:** Each node maintains its own state, no state synchronization

## Reference

- See `/docs/ClusterConfiguration.md` for complete architecture specification
- All phases follow the design described in that document
