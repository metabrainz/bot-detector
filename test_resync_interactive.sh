#!/bin/bash

# Interactive HAProxy resync test

LOG_FILE="/tmp/bot-detector-resync-test.log"

# Create config
cat > /tmp/config.yaml <<'EOF'
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
      - field_matches: { path: "/test" }
EOF

# Start bot-detector
echo "Starting bot-detector..."
./bot-detector --config-dir /tmp --log-path /dev/null > $LOG_FILE 2>&1 &
BOT_PID=$!
echo "Bot-detector PID: $BOT_PID"
echo "Log file: $LOG_FILE"
echo ""

sleep 3

# Check if running
if ! kill -0 $BOT_PID 2>/dev/null; then
    echo "ERROR: bot-detector failed to start"
    cat $LOG_FILE
    exit 1
fi

echo "✓ Bot-detector is running"
echo ""
echo "Monitoring log (Ctrl+C to stop):"
echo "================================"
tail -f $LOG_FILE | grep --line-buffered --text -E '(HEALTH_CHECK|RESYNC|SETUP)'
