#!/bin/bash

# Inject test blocked IPs into HAProxy tables manually
# This simulates IPs that bot-detector would have blocked

HAPROXY_ADDR="127.0.0.1:9999"

echo "Injecting test blocked IPs into HAProxy tables..."
echo ""

# Block 3 test IPs in the thirty_min_blocks table
TEST_IPS=(
    "192.0.2.1"
    "192.0.2.2"
    "192.0.2.3"
)

for IP in "${TEST_IPS[@]}"; do
    echo "Blocking $IP in thirty_min_blocks_ipv4..."
    echo "set table thirty_min_blocks_ipv4 key $IP data.gpc0 1" | socat stdio TCP:$HAPROXY_ADDR
done

echo ""
echo "Verifying blocks..."
echo "show table thirty_min_blocks_ipv4" | socat stdio TCP:$HAPROXY_ADDR | grep -E "(192\.0\.2\.[1-3]|# table:)"

echo ""
echo "✓ Test IPs blocked in HAProxy"
echo ""
echo "Now these IPs are in HAProxy tables but NOT in bot-detector's activity store."
echo "When you reload HAProxy, the tables will be cleared."
echo "Bot-detector will try to resync, but won't find these IPs in its state."
echo ""
echo "To properly test resync, we need IPs in BOTH places."
