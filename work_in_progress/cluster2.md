# Cluster Implementation - Remaining Phases

> **Current Status:** Phases 1-7 Complete
> **Last Updated:** 2025-11-18

This document outlines the detailed implementation plan for the remaining cluster phases (8-12).

---

## Phase 8: Error Handling and Network Resilience

**Status:** Planned (Detailed)
**Complexity:** Medium
**Dependencies:** Phases 4 (Config Polling), 6 (Metrics Collection)

### Current State Assessment

**What Already Exists:**
- ✅ Basic error logging in follower and leader
- ✅ Consecutive error tracking in MetricsCollector
- ✅ Escalating log levels (warning → error at 5 failures)
- ✅ Graceful degradation (continues polling other nodes on failure)
- ✅ Thread-safe metrics storage with RWMutex

**What's Missing for Production:**
- ❌ No retry logic with exponential backoff
- ❌ No HTTP client best practices (only basic timeout)
- ❌ Hardcoded timeouts (10s in 3 places: follower.go:59-61, 251-253; leader.go:59-61)
- ❌ Limited logging context (no retry counts, timing, error details)
- ❌ All errors treated equally (no transient vs permanent distinction)
- ❌ No circuit breaker pattern

### Detailed Implementation Plan

#### Step 1: Create Shared HTTP Client with Production Configuration
**File:** `internal/cluster/http.go` (NEW)

Create centralized HTTP client factory with production-ready transport settings:

```go
type HTTPClientConfig struct {
    Timeout              time.Duration // Overall request timeout (default: 10s)
    DialTimeout          time.Duration // Connection timeout (default: 5s)
    KeepAlive            time.Duration // TCP keep-alive (default: 30s)
    MaxIdleConns         int           // Connection pool size (default: 100)
    IdleConnTimeout      time.Duration // Idle connection timeout (default: 90s)
    TLSHandshakeTimeout  time.Duration // TLS handshake timeout (default: 10s)
    ExpectContinueTimeout time.Duration // 100-continue timeout (default: 1s)
}

func NewHTTPClient(config HTTPClientConfig) *http.Client
```

**Testing:**
- Unit test for default values
- Unit test for custom configuration
- Verify transport settings are applied

#### Step 2: Add Timeout Configuration to ClusterConfig
**Files:** `internal/cluster/types.go`, `internal/config/types.go`, `internal/config/config.go`

Add configurable timeouts to ClusterConfig:
- `HTTPTimeout time.Duration` (overall request timeout)
- `HTTPDialTimeout time.Duration` (connection timeout)

Parse from YAML with validation:
- Timeouts must be >= 1s and <= 5m
- DialTimeout must be <= Timeout

**Testing:**
- Test YAML parsing with timeout fields
- Test defaults when fields omitted
- Test validation rejects invalid values

#### Step 3: Implement Retry Logic with Exponential Backoff
**File:** `internal/cluster/retry.go` (NEW)

Create reusable retry mechanism:

```go
type RetryConfig struct {
    MaxRetries   int           // Default: 3
    InitialDelay time.Duration // Default: 1s
    MaxDelay     time.Duration // Default: 30s
    Multiplier   float64       // Default: 2.0
    Jitter       float64       // Default: 0.1 (10%)
}

func RetryWithBackoff(
    ctx context.Context,
    config RetryConfig,
    operation func() error,
    logFunc LogFunc,
    operationName string,
) error
```

**Algorithm:**
- Delay calculation: `min(initialDelay * multiplier^attempt, maxDelay)`
- Add random jitter: `actualDelay = delay * (1 ± jitter)`
- Log each retry attempt with details
- Support context cancellation

**Testing:**
- Test successful operation (no retries)
- Test retry with backoff on failures
- Test max retries reached
- Test backoff calculation and jitter
- Test context cancellation

#### Step 4: Update ConfigPoller to Use Retry Logic
**File:** `internal/cluster/follower.go`

**Changes:**
- Use shared HTTP client from Step 1
- Wrap HTTP requests in RetryWithBackoff
- Classify errors (retryable vs permanent):
  - **Retry on:** Network errors, 5xx status codes, timeout errors
  - **Don't retry:** 404, 304, 4xx client errors
- Add retry config to ConfigPollerOptions
- Enhanced logging:
  - Log retry attempt number and backoff delay
  - Log request/response timing
  - Distinguish transient vs permanent failures

**Testing:**
- Test successful poll with retry
- Test non-retryable errors (no retry)
- Test retryable errors (retries then exhausts)
- Test graceful continuation after failed poll

#### Step 5: Update MetricsCollector to Use Retry Logic
**File:** `internal/cluster/leader.go`

**Changes:**
- Use shared HTTP client from Step 1
- Wrap HTTP requests in RetryWithBackoff
- Same error classification as follower
- Add retry config to MetricsCollectorOptions
- Enhanced error tracking:
  - Track "retries exhausted" vs "permanent failure"
  - Include retry count in error messages
  - Log timing information
- Update ConsecutiveErrors logic:
  - Increment only after all retries exhausted
  - Keep existing escalation (warning at 3, error at 5)

**Testing:**
- Test successful collection with retry
- Test non-retryable errors (immediate error)
- Test retryable errors (retries then tracks)
- Test consecutive error counting with retries
- Test recovery from errors

#### Step 6: Add Detailed Cluster Logging
**Files:** `internal/cluster/follower.go`, `internal/cluster/leader.go`

**ConfigPoller Logging:**
- Log poll start with timestamp
- Log HTTP request timing (duration, response size)
- Log If-Modified-Since header value (debug)
- Log archive download size and extraction time
- Log checksum verification results
- Log cumulative statistics periodically

**MetricsCollector Logging:**
- Log collection cycle start (debug)
- Log per-node collection timing and response size
- Log successful collections with metric counts (debug)
- Log aggregated statistics periodically
- Log node health state transitions (healthy → stale → error)

**Log Levels:**
- **Debug:** Normal operations, timing, successful collections
- **Info:** State changes (startup, shutdown, node health transitions)
- **Warning:** Retryable errors, first failure, approaching threshold
- **Error:** Permanent failures, retries exhausted, critical issues

**Testing:**
- Verify log output at different levels
- Test log format consistency
- Verify timing information logged correctly

#### Step 7: Add Configuration Options to main.go
**File:** `cmd/bot-detector/main.go`

**Changes:**
- Extract HTTPTimeout and HTTPDialTimeout from cluster config
- Create HTTPClientConfig with cluster values or defaults
- Pass retry configuration to follower and leader
- Remove all hardcoded timeout values

**Defaults:**
- HTTPTimeout: 10s
- HTTPDialTimeout: 5s
- MaxRetries: 3
- InitialDelay: 1s

**Testing:**
- Integration test with cluster config containing timeouts
- Integration test with default values
- Verify timeouts actually applied to HTTP clients

#### Step 8: Update ClusterConfig Validation
**File:** `internal/cluster/types.go`

Add validation in `ClusterConfig.Validate()`:
- If HTTPTimeout set: ensure >= 1s and <= 5m
- If HTTPDialTimeout set: ensure >= 1s and <= HTTPTimeout
- Future retry config validation (if added to YAML)

**Testing:**
- Test validation accepts valid values
- Test validation rejects invalid values
- Test validation rejects dial timeout > request timeout

#### Step 9: Circuit Breaker Pattern (Optional Enhancement)
**File:** `internal/cluster/circuitbreaker.go` (NEW)

Prevent cascading failures by temporarily stopping requests to failing nodes:

**Circuit States:**
- **Closed:** Normal operation
- **Open:** Failing (skip requests for 30s)
- **HalfOpen:** Testing recovery

**Thresholds:**
- 5 consecutive failures → Open
- 2 consecutive successes in HalfOpen → Closed
- 30s timeout before HalfOpen

**Integration:**
- Add to MetricsCollector.collectFromNode()
- Check circuit state before request
- Update circuit based on result
- Log state transitions (Info level)

**Testing:**
- Test circuit opens after failures
- Test circuit recovery after success
- Test half-open state
- Test metrics collection respects circuit

#### Step 10: Documentation and Examples
**Files:** `work_in_progress/cluster.md`, `docs/ClusterConfiguration.md`, `docs/API.md`

**Updates:**
- Document timeout configuration with examples
- Document retry behavior and backoff algorithm
- Document error classification (retryable vs permanent)
- Add troubleshooting section
- Provide production-ready config example:

```yaml
cluster:
  nodes:
    - name: "node-1"
      address: "node-1.internal:8080"
    - name: "node-2"
      address: "node-2.internal:8080"
  config_poll_interval: "30s"
  metrics_report_interval: "10s"
  http_protocol: "http"
  http_timeout: "10s"          # NEW: Total request timeout
  http_dial_timeout: "5s"      # NEW: Connection timeout
```

### Summary

**Files to Create:**
- `internal/cluster/http.go` - Shared HTTP client
- `internal/cluster/retry.go` - Retry with backoff
- `internal/cluster/circuitbreaker.go` - Circuit breaker (optional)

**Files to Modify:**
- `internal/cluster/follower.go` - Add retry, enhanced logging
- `internal/cluster/leader.go` - Add retry, enhanced logging
- `internal/cluster/types.go` - Add timeout fields, validation
- `internal/config/types.go` - Add timeout fields to YAML
- `internal/config/config.go` - Parse timeout configuration
- `cmd/bot-detector/main.go` - Pass timeout/retry config
- `docs/ClusterConfiguration.md` - Document new features
- `docs/API.md` - Document cluster endpoints
- `work_in_progress/cluster.md` - Update phase status

**Testing Strategy:**
- Unit tests for each component (HTTP client, retry, circuit breaker)
- Integration tests for error scenarios
- Mock servers returning various error codes
- Test timeout and retry behavior
- Load test for connection pooling

**Value:**
Production-ready reliability with intelligent error handling, retry logic, and comprehensive logging.

---

## Phase 9: Configuration Validation and Safety

**Status:** Not Started
**Complexity:** Low
**Dependencies:** Phase 1

### What to Implement
1. Validate cluster config during load:
   - Node addresses are valid (host:port format)
   - No duplicate node names or addresses
   - Intervals are reasonable (not too short: >= 5s)
   - Own address exists in nodes list (when cluster enabled)
   - Protocol is valid ("http" or "https")
2. Add `--check` flag support for cluster config
3. Warn if node count is 1 (single-node cluster is unusual)

### Files to Create/Modify
- `internal/config/config.go` - Add cluster validation logic
- `internal/cluster/types.go` - Enhanced Validate() method
- Tests for validation edge cases

### Testing
- Test with invalid node addresses
- Test with duplicate names
- Test with duplicate addresses
- Test with intervals too short
- Test with node not in cluster list
- Test with invalid protocol

### Value
Prevents common configuration mistakes before they cause runtime issues.

---

## Phase 10: Protocol Selection (HTTP/HTTPS)

**Status:** Not Started
**Complexity:** Low
**Dependencies:** Phase 8

### What to Implement
1. Read `http_protocol` from cluster config (already implemented)
2. Update HTTP client creation in `internal/cluster/http.go` to support TLS
3. Add TLS configuration options (optional):
   - InsecureSkipVerify (for self-signed certs)
   - Custom CA certificates
   - Client certificate authentication
4. Update all URL construction to use configured protocol

### Files to Create/Modify
- `internal/cluster/http.go` - Add TLS config to HTTPClientConfig
- `internal/config/types.go` - Add TLS options to cluster config (optional)
- Tests with HTTPS (may need test certificates)

### Testing
- Test with http:// URLs
- Test with https:// URLs
- Test with self-signed certificates (InsecureSkipVerify)
- Test fallback/default behavior

### Value
Secure cluster communication for production deployments.

---

## Phase 11: Integration Testing and Documentation

**Status:** Not Started
**Complexity:** Medium
**Dependencies:** All previous phases

### What to Implement
1. Create integration test that runs 3 instances (1 leader, 2 followers)
2. Test config propagation:
   - Leader serves config archive
   - Followers download and apply config
   - Hot-reload propagates to all nodes
3. Test metrics aggregation:
   - Followers expose metrics
   - Leader collects and aggregates
   - Verify accuracy of aggregation
4. Test bootstrap process:
   - New follower with no config
   - Downloads config from leader
   - Starts successfully
5. Test failure scenarios:
   - Leader unreachable (followers continue independently)
   - Follower unreachable (leader marks as error)
   - Network partition (nodes continue, recover when restored)
6. Update README.md with cluster setup instructions
7. Create deployment examples:
   - systemd service units (leader and follower)
   - docker-compose.yml
   - Kubernetes manifests (optional)

### Files to Create/Modify
- `testing/integration/cluster_test.go` (NEW)
- `docs/ClusterSetup.md` (NEW) - Deployment guide
- `README.md` - Add cluster section with quick start
- `examples/cluster/systemd/` (NEW) - systemd units
- `examples/cluster/docker/` (NEW) - docker-compose
- `examples/cluster/configs/` (NEW) - Example YAML configs

### Testing
- Full end-to-end cluster operation (3 nodes)
- Config propagation and hot-reload
- Metrics collection and aggregation
- Bootstrap new follower
- Simulate leader failure
- Simulate follower failures
- Network partition and recovery
- Verify all documented procedures work

### Value
Validates everything works together, provides deployment guidance, enables production use.

---

## Phase 12: Optional Enhancements (Post-MVP)

**Status:** Future
**Complexity:** Varies
**Dependencies:** Phase 11

### Potential Features

1. **Metrics Visualization Dashboard (HTML/JS)**
   - Interactive web UI for cluster metrics
   - Real-time updates via WebSocket or SSE
   - Graphs and charts for trends
   - Node health overview

2. **Authentication/Authorization for Cluster Endpoints**
   - API key or token-based auth
   - TLS client certificates
   - Rate limiting per client
   - Access control lists

3. **Prometheus-Compatible Cluster Metrics**
   - `/metrics` endpoint in Prometheus format
   - Expose cluster-wide metrics
   - Per-node metrics with labels
   - Integration with Grafana

4. **Alert System for Follower Failures**
   - Configurable alert thresholds
   - Email/Slack/PagerDuty notifications
   - Alert on node down, stale, or high error rate
   - Alert aggregation and deduplication

5. **Automatic Follower Discovery**
   - Service discovery integration (Consul, etcd, Kubernetes)
   - Dynamic node registration/deregistration
   - Automatic cluster membership updates
   - Health-based node inclusion

6. **Configuration Diff Endpoint**
   - Compare current config with previous version
   - Show what changed in last reload
   - Audit trail of config changes
   - Rollback capability

7. **Cluster Health Score Calculation**
   - Weighted health metric (node health, lag, errors)
   - Overall cluster health indicator
   - Predictive alerts (degrading health)
   - Historical health tracking

### Implementation
Each feature would be its own sub-phase with separate design and testing.

### Value
Incremental improvements to cluster experience, enhanced monitoring, easier operations.

---

## Key Implementation Principles

1. **Backward Compatibility:** All cluster features are opt-in via configuration
2. **Independent Execution:** Core threat detection continues independently of cluster state
3. **Fail-Safe:** Cluster communication failures don't affect threat detection/blocking
4. **Go Idioms:** Small packages, clear interfaces, fail-fast error handling
5. **Testability:** Each phase is independently testable
6. **Incremental Value:** Each phase can be deployed to production
7. **Production Ready:** Error handling, retry logic, comprehensive logging

## Architecture Decisions

- **No automatic leader election:** Manual failover is intentional for simplicity
- **Duplicate blocks acceptable:** HAProxy handles duplicate stick-table updates gracefully
- **Pull-based metrics:** Followers expose, leader polls (avoids push complexity)
- **Independent state:** Each node maintains its own state, no state synchronization
- **Graceful degradation:** Cluster features degrade gracefully, core functionality unaffected

## Success Criteria

**Phase 8 Complete When:**
- [ ] HTTP client uses production settings (keep-alive, pooling)
- [ ] Retry logic with exponential backoff implemented
- [ ] Timeouts configurable via YAML
- [ ] Comprehensive logging with retry context
- [ ] All tests pass
- [ ] Documentation updated

**Phase 9 Complete When:**
- [ ] All cluster config validated on load
- [ ] Invalid configs rejected with clear error messages
- [ ] `--check` flag validates cluster config
- [ ] All tests pass

**Phase 10 Complete When:**
- [ ] HTTPS support works
- [ ] TLS configuration options available
- [ ] Tests with both HTTP and HTTPS pass

**Phase 11 Complete When:**
- [ ] Integration tests run 3-node cluster successfully
- [ ] All failure scenarios tested and handled
- [ ] Documentation complete and accurate
- [ ] Deployment examples provided and tested

**Ready for Production When:**
- [ ] Phases 8-11 complete
- [ ] All tests passing
- [ ] Documentation reviewed
- [ ] Deployment examples verified
- [ ] Performance tested under load

## Reference

- See `work_in_progress/cluster.md` for Phases 1-7 implementation details
- See `docs/ClusterConfiguration.md` for architecture specification
- See `docs/API.md` for cluster endpoint documentation
