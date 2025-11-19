# Block/Unblock Test Simulation

This test validates that follower nodes correctly:
1. Bootstrap and sync configuration from a leader
2. Independently detect threats using behavioral chains
3. Issue block commands for chain completions
4. Handle good actor rules (skip and optional unblock)

## Overview

This simulation tests the core blocking/unblocking functionality without requiring an actual HAProxy backend. It uses dry-run mode to verify that the follower node processes log entries correctly and generates the expected block/unblock commands.

## Test Architecture

```
┌─────────────────────┐
│   Leader Node       │
│   Port: 8080        │
│   Config: testdata/ │
└──────────┬──────────┘
           │ /config/archive
           │ (config sync)
           ▼
┌─────────────────────┐
│  Follower Node      │
│  Port: 9090         │
│  Config: follower/  │
└──────────┬──────────┘
           │
           │ Process test_logs.log
           │ (dry-run mode)
           ▼
    Block/Unblock Commands
    (logged, not executed)
```

## Test Flow

1. **Start Leader**: Uses `testdata/config.yaml` which includes:
   - `SimpleBlockChain`: 2-step chain that blocks IPs
   - `SimpleLogChain`: 2-step chain that only logs
   - Good actor rules (IP whitelist, UA patterns)

2. **Bootstrap Follower**: Starts with only `follower/FOLLOW` file
   - Fetches complete config from leader via `/config/archive`
   - Validates and extracts configuration
   - Becomes operational as follower

3. **Feed Log Entries**: Process `test_logs.log` on follower
   - Contains entries that trigger blocks
   - Contains good actor entries (skip/unblock)
   - Uses dry-run mode (no HAProxy connection)

4. **Verify Results**: Check follower output for:
   - `BLOCK!` messages for chain completions
   - `SKIP:` messages for good actors
   - Correct IP addresses and chain names

## Files

- **`test_logs.log`**: Sample HAProxy log entries for testing
- **`test-block-unblock.sh`**: Main test runner script

## Implementation Details

The test creates a temporary directory for each run containing:
- `leader/` - Leader configuration (copied from `testdata/`)
- `follower/` - Follower configuration (bootstrapped from leader)
- `live_test.log` - Empty log file that receives appended entries during test

The test uses a clever approach to simulate real-time log processing:
1. Creates an empty log file
2. Starts bot-detector tailing the empty file
3. **Appends test entries after the tailer starts**
4. Waits for buffer flushing (10 seconds)
5. Verifies block/unblock/skip behaviors

This approach is necessary because log tailers wait for new entries to be appended, rather than processing existing file contents.

## Test Scenarios

### Scenario 1: Simple Block Chain
**Trigger:** IP completes 2-step path sequence
```
IP 10.0.0.2 → GET /block/step1
IP 10.0.0.2 → GET /block/step2
```
**Expected:** `BLOCK! Chain: SimpleBlockChain completed by IP 10.0.0.2`

### Scenario 2: Good Actor Skip
**Trigger:** IP from good_actors list attempts chain
```
IP 10.10.10.5 (matches cidr:10.10.10.0/24 in good_actors_ips.txt)
IP 10.10.10.5 → GET /block/step1
IP 10.10.10.5 → GET /block/step2
```
**Expected:** `SKIP: Actor 10.10.10.5: Skipped (good_actor:our_network)` (no block)

### Scenario 3: Independent Detection
**Trigger:** Multiple different IPs complete the same chain
```
IP 10.0.0.3 → completes SimpleBlockChain
IP 10.0.0.4 → completes SimpleBlockChain
```
**Expected:** Both IPs blocked independently

## Running the Test

```bash
cd testing/block-unblock-test
./test-block-unblock.sh
```

## Success Criteria

- ✅ Follower successfully bootstraps from leader
- ✅ Follower processes all log entries
- ✅ Expected number of blocks are logged
- ✅ Good actors are skipped (not blocked)
- ✅ No errors in config sync or log processing

## Configuration Details

The test uses the existing `testdata/config.yaml` which includes:

**Behavioral Chains:**
- `SimpleBlockChain`: Blocks IPs that access `/block/step1` then `/block/step2`
- `SimpleLogChain`: Logs IPs that access `/log/step1` then `/log/step2`

**Good Actors:**
- IP range: `cidr:10.10.10.0/24` (from `good_actors_ips.txt`)
- User-Agent pattern: `regex:(?i)HealthCheck`

**Block Duration:** 1 hour (configurable in chain definition)

## Log Format

Test logs use standard HAProxy combined log format:
```
VHost IP - - [Timestamp] "Method Path Protocol" Status Size "Referer" "UserAgent"
```

Example:
```
musicbrainz.org 10.0.0.2 - - [28/Oct/2025:17:00:04 +0000] "GET /block/step1 HTTP/1.1" 200 100 "-" "TestAgent"
```

## Notes

- **Dry-run mode**: No actual HAProxy connection is made; block/unblock commands are only logged
- **Independent execution**: This test proves followers can detect threats independently of the leader
- **No timing requirements**: The test processes logs sequentially without time-based conditions
- **Configuration sync**: Validates that followers can operate with leader-provided configuration

## Extending This Test

To add more test scenarios:
1. Add new log entries to `test_logs.log`
2. Update verification logic in `test-block-unblock.sh`
3. Optionally modify `testdata/config.yaml` to add new chains (affects both this test and others)

## Troubleshooting

**Follower fails to bootstrap:**
- Check that leader is running and accessible on port 8080
- Verify the test creates the temporary directory correctly
- Check `/tmp/bot-detector-test-*/leader-output.log` for leader errors

**Blocks not appearing:**
- Ensure log entries are appended AFTER bot-detector starts (not present beforehand)
- Verify log timestamps are properly formatted: `02/Jan/2006:15:04:05 -0700`
- Verify log entries match the chain's `field_matches` criteria
- Check that IPs aren't in good_actors list
- Increase wait time after appending (currently 10 seconds for buffer flushing)

**Good actors not skipped:**
- Verify `testdata/good_actors_ips.txt` exists and contains expected ranges
- Check that test log IPs match the good_actor rules

**Test preserves tmpdir on failure:**
- When tests fail, the temporary directory is preserved for debugging
- Path is shown in output: `/tmp/bot-detector-test-XXXXXX`
- Check `processor-output.log` for full bot-detector output
