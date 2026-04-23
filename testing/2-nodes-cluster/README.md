# Two-Node Cluster Integration Test

Integration test for bot-detector cluster functionality including state synchronization.

## What It Tests

1. **Build** the application
2. **Leader startup** with persistence and state sync enabled
3. **Block IPs** by feeding log lines that trigger a chain (5 IPs × 3 429-responses each)
4. **Verify blocks** via the internal IP lookup API
5. **Cluster status** endpoint
6. **Config archive** endpoint
7. **Follower bootstrap** — downloads config from leader on first start
8. **State sync** — follower receives blocked IPs from leader
9. **Sync timing** — measures time from follower start to state sync completion

## Running

```bash
cd testing/2-nodes-cluster
./test-cluster.sh
```

## Architecture

```
Leader (:8080)                    Follower (:9090)
  - Processes test.log              - Bootstraps config from leader
  - Blocks IPs via chain match      - Syncs state via /cluster/state/merged
  - Serves merged state             - Verifies blocked IPs match leader
  - SQLite persistence              - SQLite persistence
```

## Configuration

The leader config (`leader/config.yaml`) uses:
- `version: "1.0"` with current schema
- `chain_defaults` for common chain settings
- `state_sync` enabled with 5s interval and gzip compression
- A simple `Test-Block-429` chain that blocks after 3 consecutive 429 responses
- `out_of_order_tolerance: "0s"` to disable buffering for deterministic testing

The follower (`follower/FOLLOW`) points to the leader's address for config bootstrap.
