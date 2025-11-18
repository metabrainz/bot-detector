# Two-Node Cluster Integration Test

This directory contains an integration test setup for the bot-detector cluster functionality.

## Overview

The test setup demonstrates and validates:

1. **Leader Node**: Runs with a full configuration and serves it to followers
2. **Follower Node**: Bootstraps its configuration from the leader and stays synchronized
3. **Configuration Distribution**: Via tar.gz archives with SHA256 checksums
4. **Dynamic Role Detection**: FOLLOW file monitoring for role changes

## Directory Structure

```
testing/2-nodes-cluster/
├── README.md           # This file
├── test-cluster.sh     # Automated integration test script
├── leader/
│   └── config.yaml     # Leader configuration
└── follower/
    └── FOLLOW          # Follower role marker (contains leader address)
```

## What the Test Does

The `test-cluster.sh` script performs the following steps:

1. **Build Application**: Compiles the bot-detector binary
2. **Start Leader**: Launches leader node on port 8080
3. **Test Leader Status**: Verifies leader is responding via `/cluster/status`
4. **Test Archive Endpoint**: Validates `/config/archive` serves tar.gz with checksums
5. **Bootstrap Follower**: Starts follower which auto-downloads config from leader
6. **Test Follower Status**: Verifies follower is responding
7. **Test Config Sync**: Waits for config poller to run (5s interval)
8. **Test FOLLOW Changes**: Modifies FOLLOW file to test change detection
9. **Test Role Change**: Deletes FOLLOW file to simulate follower→leader transition

## Running the Test

```bash
cd testing/2-nodes-cluster
./test-cluster.sh
```

The script will:
- Display colored output for each test step
- Show JSON responses from status endpoints
- Log archive sizes and checksums
- Keep both nodes running until you press Ctrl+C
- Automatically clean up processes on exit

## Expected Behavior

### Leader Node
- Starts without FOLLOW file (leader role)
- Serves configuration via `/config/archive` endpoint
- Provides config as tar.gz with ETag (SHA256 checksum)
- Reports role as "leader" in status endpoint

### Follower Node
- Starts with FOLLOW file pointing to leader
- Bootstraps initial config from leader on first start
- Polls leader every 5 seconds for config updates
- Reports role as "follower" in status endpoint
- Detects FOLLOW file changes and logs warnings

## Configuration Details

### Leader Config
- Port: 8080
- Cluster nodes defined: leader (8080), follower (9090)
- Config poll interval: 5s
- Single test profile for basic validation

### Follower FOLLOW File
- Contains: `http://127.0.0.1:8080`
- Indicates follower role
- Points to leader's HTTP endpoint

## Validation Points

The test validates:

✓ Leader starts and serves status endpoint
✓ Archive endpoint returns valid tar.gz
✓ Archive includes ETag header with SHA256 checksum
✓ Follower bootstraps config from leader
✓ Follower polls leader for updates
✓ FOLLOW file changes are detected
✓ FOLLOW file deletion is detected (role switch)

## Manual Testing

You can also run the nodes manually:

```bash
# Terminal 1 - Leader
cd testing/2-nodes-cluster
../../bot-detector --config leader/config.yaml --dry-run /tmp/test.log

# Terminal 2 - Follower
cd testing/2-nodes-cluster
../../bot-detector --config follower/config.yaml --dry-run /tmp/test.log --http-server :9090
```

Then test endpoints:

```bash
# Leader status
curl http://127.0.0.1:8080/cluster/status | jq

# Follower status
curl http://127.0.0.1:9090/cluster/status | jq

# Download config archive
curl -O http://127.0.0.1:8080/config/archive

# Test FOLLOW file change detection
echo "http://127.0.0.1:7070" > follower/FOLLOW
# Wait 5+ seconds and check follower logs

# Test role change (follower → leader)
rm follower/FOLLOW
# Wait 5+ seconds and check logs
```

## Troubleshooting

**Leader won't start**: Check if port 8080 is already in use
**Follower won't bootstrap**: Ensure leader is running and accessible
**Config not syncing**: Check follower logs for poller errors
**FOLLOW changes not detected**: Verify config watcher is running (5s poll interval)

## Implementation Notes

This test setup validates the cluster implementation phases:

- **Phase A**: ConfigDir and config path handling
- **Phase B**: FOLLOW file-based identity determination
- **Phase C**: SHA256 checksums in archive endpoint
- **Phase D**: Tar.gz archive distribution with dependencies
- **Phase E**: FOLLOW file change detection in ConfigWatcher

See the main repository commits for detailed implementation of each phase.
