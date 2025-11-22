#!/bin/bash

# Trigger test blocks by sending log entries to bot-detector
# This will cause bot-detector to block IPs in both its state AND HAProxy

LOG_PIPE="/tmp/bot-detector-test.fifo"

echo "Creating test log entries to trigger blocks..."
echo ""

# Create test log entries that match the TestChain
# The chain blocks IPs that access "/test"
TEST_LOGS=(
    'example.com 192.0.2.1 - - [22/Nov/2025:19:00:00 +0000] "GET /test HTTP/1.1" 200 100 "-" "TestBot/1.0"'
    'example.com 192.0.2.2 - - [22/Nov/2025:19:00:01 +0000] "GET /test HTTP/1.1" 200 100 "-" "TestBot/2.0"'
    'example.com 192.0.2.3 - - [22/Nov/2025:19:00:02 +0000] "GET /test HTTP/1.1" 200 100 "-" "TestBot/3.0"'
)

# Check if bot-detector is reading from a log file or stdin
BOT_PID=$(pgrep -f "bot-detector.*--config-dir /tmp")

if [ -z "$BOT_PID" ]; then
    echo "ERROR: bot-detector is not running"
    exit 1
fi

echo "Bot-detector PID: $BOT_PID"
echo ""
echo "Note: Current bot-detector is reading from /dev/null"
echo "We need to restart it with a real log file to inject entries."
echo ""
echo "Stopping current bot-detector..."
kill $BOT_PID
sleep 2

# Create a test log file
TEST_LOG="/tmp/test-access.log"
> $TEST_LOG

echo "Starting bot-detector with test log file..."
cd /home/zas/src/bot-detector
./bot-detector --config-dir /tmp --log-path $TEST_LOG > /tmp/bot-detector-resync-test.log 2>&1 &
NEW_PID=$!
echo "New bot-detector PID: $NEW_PID"
sleep 3

# Inject test log entries
echo ""
echo "Injecting test log entries..."
for LOG in "${TEST_LOGS[@]}"; do
    echo "$LOG" >> $TEST_LOG
    echo "  → $LOG"
    sleep 0.5
done

echo ""
echo "Waiting for blocks to be processed..."
sleep 3

echo ""
echo "Checking bot-detector log for blocks..."
grep -E "(BLOCK|TestChain)" /tmp/bot-detector-resync-test.log | tail -5

echo ""
echo "Checking HAProxy table..."
echo "show table thirty_min_blocks_ipv4" | socat stdio TCP:127.0.0.1:9999 | grep -E "(192\.0\.2\.[1-3]|# table:)"

echo ""
echo "✓ Test blocks should now be in both bot-detector state AND HAProxy tables"
echo ""
echo "Now reload HAProxy to test resync:"
echo "  sudo systemctl reload haproxy"
echo ""
echo "Watch the log:"
echo "  tail -f /tmp/bot-detector-resync-test.log | grep -E '(RESYNC|HEALTH)'"
