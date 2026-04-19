#!/bin/bash
set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$SCRIPT_DIR/../.."
BINARY="$ROOT_DIR/bot-detector"
LEADER_DIR="$SCRIPT_DIR/leader"
FOLLOWER_DIR="$SCRIPT_DIR/follower"
LOG_FILE="/tmp/bot-detector-cluster-test.log"
LEADER_STATE="$SCRIPT_DIR/leader-state"
FOLLOWER_STATE="$SCRIPT_DIR/follower-state"

PASS=0
FAIL=0

pass() { echo -e "${GREEN}✓ $1${NC}"; PASS=$((PASS + 1)); }
fail() { echo -e "${RED}✗ $1${NC}"; FAIL=$((FAIL + 1)); }

cleanup() {
    echo -e "\n${YELLOW}Cleaning up...${NC}"
    [ -n "$LEADER_PID" ] && kill "$LEADER_PID" 2>/dev/null || true
    [ -n "$FOLLOWER_PID" ] && kill "$FOLLOWER_PID" 2>/dev/null || true
    sleep 1
    rm -rf "$LOG_FILE" "$LEADER_STATE" "$FOLLOWER_STATE" "$SCRIPT_DIR/leader.log" "$SCRIPT_DIR/follower.log"
    rm -f "$FOLLOWER_DIR/config.yaml"
}
trap cleanup EXIT

echo -e "${GREEN}=== Bot Detector Cluster Integration Test ===${NC}\n"

# --- Build ---
echo -e "${YELLOW}Step 1: Building...${NC}"
cd "$ROOT_DIR"
go build -o bot-detector ./cmd/bot-detector
pass "Build successful"
echo ""

# --- Create empty log file ---
echo -e "${YELLOW}Step 2: Preparing test log...${NC}"
> "$LEADER_DIR/test.log"
pass "Created empty log file"
echo ""

# --- Start leader ---
echo -e "${YELLOW}Step 3: Starting leader...${NC}"
mkdir -p "$LEADER_STATE"
"$BINARY" \
    --config-dir "$LEADER_DIR" \
    --listen 127.0.0.1:8080 \
    --state-dir "$LEADER_STATE" \
    --cluster-node-name leader \
    > "$SCRIPT_DIR/leader.log" 2>&1 &
LEADER_PID=$!
sleep 2

if kill -0 "$LEADER_PID" 2>/dev/null; then
    pass "Leader started (PID: $LEADER_PID)"
else
    fail "Leader failed to start"
    cat "$SCRIPT_DIR/leader.log"
    exit 1
fi
echo ""

# --- Feed log lines to trigger blocks ---
echo -e "${YELLOW}Step 4: Feeding log lines to block 5 IPs...${NC}"
TS=$(LC_ALL=C date -u +"%d/%b/%Y:%H:%M:%S +0000")
for i in $(seq 1 5); do
    for j in $(seq 1 3); do
        echo "example.com 10.0.0.$i - - [$TS] \"GET /page/$j HTTP/1.1\" 429 568 \"-\" \"TestBot/1.0\"" >> "$LOG_FILE"
    done
done
sleep 2

BLOCKED=0
for i in $(seq 1 5); do
    STATUS=$(curl -s "http://127.0.0.1:8080/api/v1/cluster/internal/ip/10.0.0.$i" 2>/dev/null || echo '{}')
    if echo "$STATUS" | grep -q '"blocked"'; then
        BLOCKED=$((BLOCKED + 1))
    fi
done
if [ "$BLOCKED" -ge 4 ]; then
    pass "Leader blocked $BLOCKED/5 IPs"
else
    fail "Leader only blocked $BLOCKED/5 IPs (expected ≥4)"
    grep -E "BLOCK|PARSE|UNKNOWN" "$SCRIPT_DIR/leader.log" 2>/dev/null | tail -5 | sed 's/^/    /'
fi
echo ""

# --- Test cluster status ---
echo -e "${YELLOW}Step 5: Testing leader cluster status...${NC}"
CLUSTER_STATUS=$(curl -s http://127.0.0.1:8080/api/v1/cluster/status 2>/dev/null || echo '{}')
if echo "$CLUSTER_STATUS" | grep -q '"role":"leader"'; then
    pass "Leader reports role=leader"
else
    fail "Leader cluster status unexpected: $CLUSTER_STATUS"
fi
echo ""

# --- Test config archive ---
echo -e "${YELLOW}Step 6: Testing config archive endpoint...${NC}"
ARCHIVE_SIZE=$(curl -s http://127.0.0.1:8080/config/archive 2>/dev/null | wc -c)
if [ "$ARCHIVE_SIZE" -gt 100 ]; then
    pass "Config archive endpoint working ($ARCHIVE_SIZE bytes)"
else
    fail "Config archive too small ($ARCHIVE_SIZE bytes)"
fi
echo ""

# --- Start follower ---
echo -e "${YELLOW}Step 7: Starting follower with state sync...${NC}"
mkdir -p "$FOLLOWER_STATE"
rm -f "$FOLLOWER_DIR/config.yaml"

SYNC_START=$(date +%s%N)
"$BINARY" \
    --config-dir "$FOLLOWER_DIR" \
    --listen 127.0.0.1:9090 \
    --state-dir "$FOLLOWER_STATE" \
    --cluster-node-name follower \
    > "$SCRIPT_DIR/follower.log" 2>&1 &
FOLLOWER_PID=$!
sleep 3

if kill -0 "$FOLLOWER_PID" 2>/dev/null; then
    pass "Follower started (PID: $FOLLOWER_PID)"
else
    fail "Follower failed to start"
    cat "$SCRIPT_DIR/follower.log"
    exit 1
fi
echo ""

# --- Verify follower bootstrapped config ---
echo -e "${YELLOW}Step 8: Verifying follower config bootstrap...${NC}"
if [ -f "$FOLLOWER_DIR/config.yaml" ]; then
    CONFIG_SIZE=$(wc -c < "$FOLLOWER_DIR/config.yaml")
    pass "Follower bootstrapped config ($CONFIG_SIZE bytes)"
else
    fail "Follower did not bootstrap config"
fi
echo ""

# --- Wait for state sync and verify ---
echo -e "${YELLOW}Step 9: Waiting for state sync...${NC}"
MAX_WAIT=30
SYNCED=false
for attempt in $(seq 1 $MAX_WAIT); do
    SYNC_BLOCKED=0
    for i in $(seq 1 5); do
        STATUS=$(curl -s "http://127.0.0.1:9090/api/v1/cluster/internal/ip/10.0.0.$i" 2>/dev/null || echo '{}')
        if echo "$STATUS" | grep -q '"blocked"'; then
            SYNC_BLOCKED=$((SYNC_BLOCKED + 1))
        fi
    done
    if [ "$SYNC_BLOCKED" -ge 4 ]; then
        SYNC_END=$(date +%s%N)
        SYNC_MS=$(( (SYNC_END - SYNC_START) / 1000000 ))
        pass "Follower synced $SYNC_BLOCKED/5 blocked IPs (${SYNC_MS}ms from follower start)"
        SYNCED=true
        break
    fi
    sleep 1
done
if [ "$SYNCED" = false ]; then
    fail "Follower did not sync blocked IPs within ${MAX_WAIT}s (got $SYNC_BLOCKED/5)"
    echo "  Follower log tail:"
    grep -E "STATE_SYNC|BLOCK|ERROR" "$SCRIPT_DIR/follower.log" 2>/dev/null | tail -10 | sed 's/^/    /'
fi
echo ""

# --- Show state sync metrics ---
echo -e "${YELLOW}Step 10: State sync metrics...${NC}"
SYNC_LINE=$(grep "STATE_SYNC.*Merged from leader\|STATE_SYNC.*Fetched initial state" "$SCRIPT_DIR/follower.log" 2>/dev/null | tail -1)
if [ -n "$SYNC_LINE" ]; then
    pass "State sync completed"
    echo "  $SYNC_LINE"
else
    SYNC_LINE=$(grep "STATE_SYNC" "$SCRIPT_DIR/follower.log" 2>/dev/null | tail -3)
    if [ -n "$SYNC_LINE" ]; then
        fail "State sync may not have completed"
        echo "$SYNC_LINE" | sed 's/^/    /'
    else
        fail "No state sync activity found in follower logs"
    fi
fi
echo ""

# --- Summary ---
echo -e "${GREEN}=== Results: $PASS passed, $FAIL failed ===${NC}"
[ "$FAIL" -gt 0 ] && exit 1
exit 0
