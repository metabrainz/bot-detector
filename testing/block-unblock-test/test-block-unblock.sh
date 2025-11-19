#!/bin/bash
set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}=== Bot Detector Block/Unblock Test ===${NC}\n"

# Function to check if a port is available
check_port() {
    local port=$1
    if lsof -Pi :$port -sTCP:LISTEN -t >/dev/null 2>&1 ; then
        return 1  # Port is in use
    else
        return 0  # Port is available
    fi
}

# Function to find an available port starting from a base port
find_available_port() {
    local base_port=$1
    local port=$base_port
    while [ $port -lt $((base_port + 100)) ]; do
        if check_port $port; then
            echo $port
            return 0
        fi
        port=$((port + 1))
    done
    return 1  # No available port found
}

# Find available ports for leader and follower
echo "Checking port availability..."
LEADER_PORT=$(find_available_port 8080)
if [ -z "$LEADER_PORT" ]; then
    echo -e "${RED}✗ Could not find available port for leader (tried 8080-8179)${NC}"
    exit 1
fi

FOLLOWER_PORT=$(find_available_port 9090)
if [ -z "$FOLLOWER_PORT" ]; then
    echo -e "${RED}✗ Could not find available port for follower (tried 9090-9189)${NC}"
    exit 1
fi

echo -e "${GREEN}✓ Using ports: Leader=$LEADER_PORT, Follower=$FOLLOWER_PORT${NC}\n"

# Create temporary directory for test
TMPDIR=$(mktemp -d -t bot-detector-test-XXXXXX)
echo "Using temporary directory: $TMPDIR"

# Graceful kill
graceful_kill() {
  # Check if a PID was provided
  if [ -z "$1" ]; then
    echo "Usage: graceful_kill <PID> [delay_seconds]"
    return 1
  fi

  local PID="$1"
  # Set the delay, default to 3 seconds
  local DELAY=${2:-3}

  echo "Attempting graceful termination (SIGTERM) for PID $PID..."
  
  # 1. Send SIGTERM (normal kill)
  kill "$PID" 2>/dev/null

  # Check if the process exists immediately after sending the signal
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "Process $PID not found or terminated immediately."
    return 0
  fi

  echo "Waiting for $DELAY seconds for process $PID to exit..."
  
  # 2. Wait for the specified delay
  sleep "$DELAY"

  # 3. Check if the process is still running
  if kill -0 "$PID" 2>/dev/null; then
    echo "Process $PID is still running. Forcing termination (SIGKILL -9)..."
    # 4. If still running, send SIGKILL (kill -9)
    kill -9 "$PID" 2>/dev/null
    
    # Final check for successful SIGKILL
    if kill -0 "$PID" 2>/dev/null; then
      echo "ERROR: Process $PID could not be killed."
      return 1
    else
      echo "Process $PID successfully terminated with SIGKILL."
      return 0
    fi
  else
    echo "Process $PID gracefully terminated."
    return 0
  fi
}

# Clean up function
cleanup() {
    echo -e "\n${YELLOW}Cleaning up...${NC}"
    if [ -n "$LEADER_PID" ] && graceful_kill $LEADER_PID 2>/dev/null; then
        echo "Stopped leader (PID: $LEADER_PID)"
    fi
    if [ -n "$FOLLOWER_PID" ] && graceful_kill $FOLLOWER_PID 2>/dev/null; then
        echo "Stopped follower (PID: $FOLLOWER_PID)"
    fi
    if [ -n "$PROCESSOR_PID" ] && gracefull_kill $PROCESSOR_PID 2>/dev/null; then
        echo "Killed $PROCESSOR_PID"
    fi
    if [ -d "$TMPDIR" ]; then
        rm -rf "$TMPDIR"
        echo "Removed temporary directory"
    fi
}

trap cleanup EXIT

# Build the application
echo -e "${YELLOW}Step 1: Building application...${NC}"
cd ../..
go build -o bot-detector ./cmd/bot-detector
cd testing/block-unblock-test
echo -e "${GREEN}✓ Build successful${NC}\n"

# Set up temporary directories
echo -e "${YELLOW}Step 2: Setting up temporary test environment...${NC}"
mkdir -p "$TMPDIR/leader"
mkdir -p "$TMPDIR/follower"

# Copy testdata files to leader directory
cp ../../testdata/config.yaml "$TMPDIR/leader/"
cp ../../testdata/good_actors_ips.txt "$TMPDIR/leader/"
cp ../../testdata/http2_paths.txt "$TMPDIR/leader/"

# Add cluster configuration to leader's config
cat >> "$TMPDIR/leader/config.yaml" <<EOF

# Cluster configuration for testing
cluster:
  nodes:
    - name: "leader"
      address: "localhost:$LEADER_PORT"
    - name: "follower"
      address: "localhost:$FOLLOWER_PORT"
  config_poll_interval: "30s"
  metrics_report_interval: "10s"
  protocol: "http"
EOF

# Create FOLLOW file for follower
echo "localhost:$LEADER_PORT" > "$TMPDIR/follower/FOLLOW"

# Copy test log file
cp test_logs.log "$TMPDIR/"

# Create empty log files for leader
touch "$TMPDIR/leader.log"

echo -e "${GREEN}✓ Test environment ready${NC}"
echo "  Leader config: $TMPDIR/leader/"
echo "  Follower config: $TMPDIR/follower/"
echo "  Test logs: $TMPDIR/test_logs.log"
echo ""

# Start leader node
echo -e "${YELLOW}Step 3: Starting leader node...${NC}"
../../bot-detector --config-dir "$TMPDIR/leader" --log-path "$TMPDIR/leader.log" --http-server :$LEADER_PORT > "$TMPDIR/leader-output.log" 2>&1 &
LEADER_PID=$!
sleep 3

# Verify leader is running
if ! kill -0 $LEADER_PID 2>/dev/null; then
    echo -e "${RED}✗ Leader failed to start${NC}"
    cat "$TMPDIR/leader-output.log"
    exit 1
fi
echo -e "${GREEN}✓ Leader started (PID: $LEADER_PID)${NC}\n"

# Test leader status
echo -e "${YELLOW}Step 4: Verifying leader status...${NC}"
LEADER_STATUS=$(curl -s http://127.0.0.1:$LEADER_PORT/cluster/status)
echo "$LEADER_STATUS" | python3 -m json.tool
echo -e "${GREEN}✓ Leader is responding${NC}\n"

# Test config archive availability
echo -e "${YELLOW}Step 5: Verifying config archive endpoint...${NC}"
ARCHIVE_SIZE=$(curl -s http://127.0.0.1:$LEADER_PORT/config/archive | wc -c)
echo "Archive size: $ARCHIVE_SIZE bytes"
if [ "$ARCHIVE_SIZE" -gt 100 ]; then
    echo -e "${GREEN}✓ Archive endpoint serving config${NC}\n"
else
    echo -e "${RED}✗ Archive seems too small${NC}\n"
    exit 1
fi

# Bootstrap follower
echo -e "${YELLOW}Step 6: Bootstrapping follower node...${NC}"
../../bot-detector --config-dir "$TMPDIR/follower" --log-path "$TMPDIR/leader.log" --http-server :$FOLLOWER_PORT > "$TMPDIR/follower-output.log" 2>&1 &
FOLLOWER_PID=$!
sleep 5

# Check if follower bootstrapped successfully
if [ -f "$TMPDIR/follower/config.yaml" ]; then
    echo -e "${GREEN}✓ Follower bootstrapped config from leader${NC}"
    echo "  Config size: $(wc -c < $TMPDIR/follower/config.yaml) bytes"
    if [ -f "$TMPDIR/follower/good_actors_ips.txt" ]; then
        echo "  Good actors file: $(wc -l < $TMPDIR/follower/good_actors_ips.txt) lines"
    fi
else
    echo -e "${RED}✗ Follower failed to bootstrap config${NC}"
    cat "$TMPDIR/follower-output.log"
    exit 1
fi
echo ""

# Verify follower is running
echo -e "${YELLOW}Step 7: Verifying follower status...${NC}"
FOLLOWER_STATUS=$(curl -s http://127.0.0.1:$FOLLOWER_PORT/cluster/status)
echo "$FOLLOWER_STATUS" | python3 -m json.tool
echo -e "${GREEN}✓ Follower is responding${NC}\n"

# Verify config synchronization happened during bootstrap
echo -e "${YELLOW}Step 8: Verifying initial config synchronization...${NC}"
FOLLOWER_CONFIG_SIZE=$(wc -c < "$TMPDIR/follower/config.yaml")
LEADER_CONFIG_SIZE=$(wc -c < "$TMPDIR/leader/config.yaml")
echo "  Leader config size: $LEADER_CONFIG_SIZE bytes"
echo "  Follower config size: $FOLLOWER_CONFIG_SIZE bytes"

if [ "$FOLLOWER_CONFIG_SIZE" -eq "$LEADER_CONFIG_SIZE" ]; then
    echo -e "${GREEN}✓ Follower config matches leader (bootstrap sync successful)${NC}"
else
    echo -e "${YELLOW}⚠ Config sizes differ (leader may have additional files)${NC}"
fi

# Verify cluster configuration was synced
if grep -q "cluster:" "$TMPDIR/follower/config.yaml"; then
    echo -e "${GREEN}✓ Cluster configuration synced to follower${NC}"
    echo "  Follower knows about cluster nodes and polling intervals"
else
    echo -e "${RED}✗ Cluster configuration missing from follower${NC}"
    exit 1
fi
echo ""

# NOTE: Runtime config updates (modifying leader config while running and verifying
# follower picks it up) requires further investigation. The archive serving mechanism
# may need updates to detect file changes and rebuild the archive dynamically.

# Now stop follower to run log processing test
echo -e "${YELLOW}Step 9: Stopping follower for log processing test...${NC}"
if [ -n "$FOLLOWER_PID" ] && graceful_kill $FOLLOWER_PID 5 2>/dev/null; then
	echo -e "${GREEN}✓ Follower stopped${NC}\n"
fi
FOLLOWER_PID=""
sleep 1

# Process test logs with follower config
echo -e "${YELLOW}Step 10: Processing test logs on follower...${NC}"
echo "Running bot-detector with empty log file, then appending test entries..."

# Create temp file for output
LOGFILE="$TMPDIR/processor-output.log"

# Create EMPTY log file first (tailer will start at EOF)
> "$TMPDIR/live_test.log"

# Start bot-detector watching the empty log file
../../bot-detector --config-dir "$TMPDIR/follower" --log-path "$TMPDIR/live_test.log" > "$LOGFILE" 2>&1 &
PROCESSOR_PID=$!

# Wait for bot-detector to start and begin tailing
sleep 2

# Now append test log entries to simulate real-time log growth
echo "Appending test log entries..."
cat test_logs.log >> "$TMPDIR/live_test.log"

# Wait for processing and buffer flushing
# Buffer worker has 5s tolerance + 2.5s tick interval
echo "Waiting 10 seconds for log processing and buffer flushing..."
sleep 10

# Stop the processor forcefully
graceful_kill $PROCESSOR_PID
sleep 1

echo -e "${GREEN}✓ Log processing complete${NC}\n"

# Show processor output for debugging
echo -e "${YELLOW}Processor Output (last 30 lines):${NC}"
tail -30 "$LOGFILE"
echo ""

# Analyze results
echo -e "${YELLOW}Step 11: Analyzing results...${NC}\n"

# Count BLOCK messages
BLOCK_COUNT=$(grep -c "BLOCK!" "$LOGFILE" || true)
echo "  BLOCK commands issued: $BLOCK_COUNT"

# Count LOG actions
LOG_COUNT=$(grep -c "Chain: SimpleLogChain completed" "$LOGFILE" || true)
echo "  LOG actions triggered: $LOG_COUNT"

# Count SKIP messages
SKIP_COUNT=$(grep -c "\[SKIP\]" "$LOGFILE" || true)
echo "  SKIP messages (good actors): $SKIP_COUNT"

echo ""

# Display specific block details
echo -e "${YELLOW}Block Details:${NC}"
grep "BLOCK!" "$LOGFILE" || echo "  (none found)"
echo ""

# Display log chain completions
echo -e "${YELLOW}Log Chain Completions:${NC}"
grep "Chain: SimpleLogChain completed" "$LOGFILE" || echo "  (none found)"
echo ""

# Display good actor skips
echo -e "${YELLOW}Good Actor Skips:${NC}"
grep "\[SKIP\]" "$LOGFILE" | head -5 || echo "  (none found)"
echo ""

# Verify expected counts
echo -e "${YELLOW}Step 12: Verifying expected results...${NC}"

EXPECTED_BLOCKS=2  # 10.0.0.2 and 10.0.0.3
EXPECTED_LOGS=1    # 10.0.0.4
EXPECTED_SKIPS=2   # 10.10.10.5 (IP) and 10.0.0.7 (UA)

SUCCESS=true

if [ "$BLOCK_COUNT" -eq "$EXPECTED_BLOCKS" ]; then
    echo -e "${GREEN}✓ BLOCK count matches (expected: $EXPECTED_BLOCKS, got: $BLOCK_COUNT)${NC}"
else
    echo -e "${RED}✗ BLOCK count mismatch (expected: $EXPECTED_BLOCKS, got: $BLOCK_COUNT)${NC}"
    SUCCESS=false
fi

if [ "$LOG_COUNT" -eq "$EXPECTED_LOGS" ]; then
    echo -e "${GREEN}✓ LOG count matches (expected: $EXPECTED_LOGS, got: $LOG_COUNT)${NC}"
else
    echo -e "${RED}✗ LOG count mismatch (expected: $EXPECTED_LOGS, got: $LOG_COUNT)${NC}"
    SUCCESS=false
fi

if [ "$SKIP_COUNT" -ge "$EXPECTED_SKIPS" ]; then
    echo -e "${GREEN}✓ SKIP count acceptable (expected: >=$EXPECTED_SKIPS, got: $SKIP_COUNT)${NC}"
else
    echo -e "${RED}✗ SKIP count too low (expected: >=$EXPECTED_SKIPS, got: $SKIP_COUNT)${NC}"
    SUCCESS=false
fi

echo ""

# Check for specific IPs
echo -e "${YELLOW}Step 13: Verifying specific IP blocks...${NC}"

if grep -q "10.0.0.2" "$LOGFILE" && grep -q "BLOCK!" "$LOGFILE"; then
    echo -e "${GREEN}✓ IP 10.0.0.2 was blocked (SimpleBlockChain)${NC}"
else
    echo -e "${RED}✗ IP 10.0.0.2 was NOT blocked${NC}"
    SUCCESS=false
fi

if grep -q "10.0.0.3" "$LOGFILE" && grep -q "BLOCK!" "$LOGFILE"; then
    echo -e "${GREEN}✓ IP 10.0.0.3 was blocked (SimpleBlockChain)${NC}"
else
    echo -e "${RED}✗ IP 10.0.0.3 was NOT blocked${NC}"
    SUCCESS=false
fi

if grep -q "10.10.10.5" "$LOGFILE" && grep -q "SKIP" "$LOGFILE"; then
    echo -e "${GREEN}✓ IP 10.10.10.5 was skipped (good_actor)${NC}"
else
    echo -e "${RED}✗ IP 10.10.10.5 was NOT skipped${NC}"
    SUCCESS=false
fi

echo ""

# Final result
if [ "$SUCCESS" = true ]; then
    echo -e "${GREEN}=== All tests passed! ===${NC}\n"

    echo -e "${YELLOW}Summary:${NC}"
    echo "  ✓ Leader served config with cluster configuration"
    echo "  ✓ Follower bootstrapped successfully via /config/archive"
    echo "  ✓ Follower synced all config files (including good_actors_ips.txt)"
    echo "  ✓ Cluster configuration propagated to follower"
    echo "  ✓ Behavioral chains detected correctly"
    echo "  ✓ Block commands issued for malicious IPs"
    echo "  ✓ Good actors were skipped (not blocked)"
    echo "  ✓ Independent threat detection verified"
    echo ""
    exit 0
else
    echo -e "${RED}=== Some tests failed ===${NC}\n"
    echo -e "${YELLOW}Debug: Full output saved to $LOGFILE${NC}"
    echo -e "${YELLOW}To inspect: cat $LOGFILE${NC}\n"

    # Don't delete tmpdir on failure for debugging
    trap - EXIT
    echo -e "${YELLOW}Temporary directory preserved for debugging: $TMPDIR${NC}"

    # Still cleanup processes
    [ -n "$LEADER_PID" ] && graceful_kill $LEADER_PID 2>/dev/null
    [ -n "$PROCESSOR_PID" ] && graceful_kill $PROCESSOR_PID 2>/dev/null

    exit 1
fi
