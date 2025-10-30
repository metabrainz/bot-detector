package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// BlockIPForDuration sends a block command to the HAProxy socket and checks the response.
func BlockIPForDuration(ip string, duration time.Duration) error {
	if DryRun {
		LogOutput(LevelInfo, "DRYRUN", "Would block IP %s for %v (Chain complete).", ip, duration)
		return nil
	}

	haproxyDuration := fmt.Sprintf("%.0fs", duration.Seconds())
	command := fmt.Sprintf("set map %s %s true timeout %s\n", BlockedMapPath, ip, haproxyDuration)

	conn, err := net.Dial("unix", HAProxySocketPath)
	if err != nil {
		LogOutput(LevelError, "ERROR", "Failed to connect to HAProxy socket %s during block attempt for IP %s: %v", HAProxySocketPath, ip, err)
		LogOutput(LevelWarning, "FAILSAFE", "Block for IP %s downgraded to LOG action.", ip)
		return nil
	}
	defer conn.Close()

	if _, err = conn.Write([]byte(command)); err != nil {
		LogOutput(LevelError, "ERROR", "Failed to send command to HAProxy for IP %s: %v", ip, err)
		return nil
	}

	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')

	if err != nil && !errors.Is(err, io.EOF) {
		LogOutput(LevelError, "ERROR", "HAProxy response read error for IP %s: %v", ip, err)
		return nil
	}

	trimmedResponse := strings.TrimSpace(response)

	if strings.HasPrefix(trimmedResponse, "500") || strings.Contains(trimmedResponse, "error") {
		LogOutput(LevelError, "HAPROXY_ERR", "HAProxy execution failed for IP %s. Response: %s", ip, trimmedResponse)
		return nil
	}

	LogOutput(LevelCritical, "HAPROXY_BLOCK", "IP %s blocked for %v (via map: %s)", ip, duration, BlockedMapPath)
	return nil
}