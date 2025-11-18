#!/bin/bash
set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}=== Bot Detector Cluster Integration Test ===${NC}\n"

# Clean up function
cleanup() {
    echo -e "\n${YELLOW}Cleaning up...${NC}"
    if [ -n "$LEADER_PID" ]; then
        kill $LEADER_PID 2>/dev/null || true
        echo "Stopped leader (PID: $LEADER_PID)"
    fi
    if [ -n "$FOLLOWER_PID" ]; then
        kill $FOLLOWER_PID 2>/dev/null || true
        echo "Stopped follower (PID: $FOLLOWER_PID)"
    fi
    rm -f /tmp/bot-detector-test.log
}

trap cleanup EXIT

# Build the application
echo -e "${YELLOW}Step 1: Building application...${NC}"
cd ../..
go build -o bot-detector ./cmd/bot-detector
cd testing/2-nodes-cluster
echo -e "${GREEN}✓ Build successful${NC}\n"

# Start leader node
echo -e "${YELLOW}Step 2: Starting leader node...${NC}"
../../bot-detector --config leader/config.yaml --dry-run /tmp/bot-detector-test.log &
LEADER_PID=$!
sleep 3
echo -e "${GREEN}✓ Leader started (PID: $LEADER_PID)${NC}\n"

# Test leader status
echo -e "${YELLOW}Step 3: Testing leader status endpoint...${NC}"
LEADER_STATUS=$(curl -s http://127.0.0.1:8080/cluster/status)
echo "$LEADER_STATUS" | python3 -m json.tool
echo -e "${GREEN}✓ Leader is responding${NC}\n"

# Test archive endpoint
echo -e "${YELLOW}Step 4: Testing config archive endpoint...${NC}"
curl -s -I http://127.0.0.1:8080/config/archive | grep -E "(HTTP|Content-Type|Last-Modified|ETag)"
ARCHIVE_SIZE=$(curl -s http://127.0.0.1:8080/config/archive | wc -c)
echo "Archive size: $ARCHIVE_SIZE bytes"
if [ "$ARCHIVE_SIZE" -gt 100 ]; then
    echo -e "${GREEN}✓ Archive endpoint working${NC}\n"
else
    echo -e "${RED}✗ Archive seems too small${NC}\n"
    exit 1
fi

# Test bootstrap functionality
echo -e "${YELLOW}Step 5: Testing follower bootstrap...${NC}"
# Remove any existing config in follower directory
rm -f follower/config.yaml
# The follower will bootstrap on startup
../../bot-detector --config follower/config.yaml --dry-run /tmp/bot-detector-test.log --http-server :9090 &
FOLLOWER_PID=$!
sleep 5

# Check if follower bootstrapped config
if [ -f follower/config.yaml ]; then
    echo -e "${GREEN}✓ Follower bootstrapped config from leader${NC}"
    echo "Config file size: $(wc -c < follower/config.yaml) bytes"
else
    echo -e "${RED}✗ Follower failed to bootstrap config${NC}"
    exit 1
fi
echo ""

# Test follower status
echo -e "${YELLOW}Step 6: Testing follower status endpoint...${NC}"
FOLLOWER_STATUS=$(curl -s http://127.0.0.1:9090/cluster/status)
echo "$FOLLOWER_STATUS" | python3 -m json.tool
echo -e "${GREEN}✓ Follower is responding${NC}\n"

# Test config synchronization
echo -e "${YELLOW}Step 7: Testing config synchronization...${NC}"
echo "Waiting 6 seconds for config poll..."
sleep 6
echo -e "${GREEN}✓ Config poller should have run (check logs above)${NC}\n"

# Test FOLLOW file detection
echo -e "${YELLOW}Step 8: Testing FOLLOW file change detection...${NC}"
echo "Modifying FOLLOW file to point to different leader..."
echo "http://127.0.0.1:7070" > follower/FOLLOW
echo "Waiting 6 seconds for change detection..."
sleep 6
echo -e "${GREEN}✓ FOLLOW file change should be detected (check logs above)${NC}\n"

echo -e "${YELLOW}Step 9: Testing FOLLOW file deletion (role change)...${NC}"
rm follower/FOLLOW
echo "Waiting 6 seconds for change detection..."
sleep 6
echo -e "${GREEN}✓ FOLLOW file deletion should be detected (check logs above)${NC}\n"

echo -e "${GREEN}=== All tests completed successfully! ===${NC}"
echo -e "\nLeader PID: $LEADER_PID"
echo -e "Follower PID: $FOLLOWER_PID"
echo -e "\nPress Ctrl+C to stop the nodes and exit"

# Keep script running
wait
