#!/bin/bash

# Test script for HAProxy backend resync functionality
# This simulates bot-detector detecting and resyncing after HAProxy restart/reload

set -e

HAPROXY_ADDR="127.0.0.1:9999"
CONFIG_DIR="./testdata"
LOG_FILE="/tmp/bot-detector-resync-test.log"

echo "=== HAProxy Resync Test ==="
echo ""

# Check HAProxy is accessible
echo "1. Checking HAProxy connection..."
if echo "show info" | socat stdio TCP:$HAPROXY_ADDR | grep -q "Uptime_sec:"; then
    UPTIME=$(echo "show info" | socat stdio TCP:$HAPROXY_ADDR | grep "^Uptime_sec:" | awk '{print $2}')
    echo "   ✓ HAProxy is running (uptime: ${UPTIME}s)"
else
    echo "   ✗ Cannot connect to HAProxy at $HAPROXY_ADDR"
    exit 1
fi

# Check available tables
echo ""
echo "2. Checking HAProxy tables..."
TABLES=$(echo "show table" | socat stdio TCP:$HAPROXY_ADDR | grep "^# table:" | wc -l)
echo "   Found $TABLES stick tables"

# Create a minimal test config
echo ""
echo "3. Creating test configuration..."
cat > /tmp/config.yaml <<EOF
version: "1.0"

application:
  log_level: "info"

blockers:
  default_duration: "30m"
  commands_per_second: 100
  command_queue_size: 1000
  backends:
    haproxy:
      addresses:
        - "127.0.0.1:9999"
      duration_tables:
        "30m": "thirty_min_blocks"
        "1h": "one_hour_blocks"

chains:
  - name: "TestChain"
    action: block
    block_duration: 30m
    match_key: ip
    steps:
      - field_matches:
          path: "/test"
EOF

echo "   ✓ Config created at /tmp/config.yaml"

# Start bot-detector in background
echo ""
echo "4. Starting bot-detector..."
echo "   Log file: $LOG_FILE"
./bot-detector --config-dir /tmp --log-path /dev/null > $LOG_FILE 2>&1 &
BOT_PID=$!
echo "   ✓ bot-detector started (PID: $BOT_PID)"

# Wait for initialization
sleep 2

# Check if bot-detector is running
if ! kill -0 $BOT_PID 2>/dev/null; then
    echo "   ✗ bot-detector failed to start"
    cat $LOG_FILE
    exit 1
fi

echo ""
echo "5. Monitoring health checks..."
echo "   Watching for HEALTH_CHECK messages..."
sleep 6  # Wait for at least one health check

# Show initial health check
grep "HEALTH_CHECK" $LOG_FILE | tail -1 || echo "   (no health checks yet)"

echo ""
echo "=== Ready for Testing ==="
echo ""
echo "Bot-detector is now running and monitoring HAProxy."
echo "The health checker runs every 5 seconds."
echo ""
echo "Test scenarios:"
echo ""
echo "  A) Test RELOAD detection:"
echo "     Run: sudo systemctl reload haproxy"
echo "     OR:  sudo haproxy -f /etc/haproxy/haproxy.cfg -sf \$(pidof haproxy)"
echo ""
echo "  B) Test RESTART detection:"
echo "     Run: sudo systemctl restart haproxy"
echo ""
echo "  C) Test STOP/START (recovery):"
echo "     Run: sudo systemctl stop haproxy"
echo "     Wait 10 seconds..."
echo "     Run: sudo systemctl start haproxy"
echo ""
echo "After each action, watch the log file:"
echo "  tail -f $LOG_FILE | grep -E '(HEALTH_CHECK|RESYNC)'"
echo ""
echo "When done testing, press ENTER to stop bot-detector..."
read

echo ""
echo "Stopping bot-detector (PID: $BOT_PID)..."
kill $BOT_PID 2>/dev/null || true
wait $BOT_PID 2>/dev/null || true

echo ""
echo "=== Test Summary ==="
echo ""
echo "Health checks detected:"
grep -c "HEALTH_CHECK" $LOG_FILE || echo "0"

echo ""
echo "Backend restarts detected:"
grep -c "restarted/reloaded" $LOG_FILE || echo "0"

echo ""
echo "Backend recoveries detected:"
grep -c "recovered and is now healthy" $LOG_FILE || echo "0"

echo ""
echo "Resyncs triggered:"
grep -c "Starting resync" $LOG_FILE || echo "0"

echo ""
echo "Full log available at: $LOG_FILE"
