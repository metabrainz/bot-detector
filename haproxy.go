package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// blockIPOnInstance connects to a single HAProxy instance over TCP and executes the command.
func blockIPOnInstance(addr, ip, command string) error {
	// Use a short timeout to prevent connection hangs
	const dialTimeout = 5 * time.Second

	// 1. Connection attempt
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return fmt.Errorf("failed to connect to HAProxy instance %s: %w", addr, err)
	}
	defer conn.Close()

	// 2. Write attempt
	if _, err = conn.Write([]byte(command)); err != nil {
		return fmt.Errorf("failed to send command to HAProxy instance %s: %w", addr, err)
	}

	// 3. Read attempt (Command Error Check)
	reader := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(dialTimeout))
	response, err := reader.ReadString('\n')

	// EOF is often expected after a response, but other read errors are reported
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("HAProxy response read error from %s: %w", addr, err)
	}

	trimmedResponse := strings.TrimSpace(response)

	if trimmedResponse != "" {
		// HAProxy returned a non-empty string, indicating a command syntax or runtime error
		return fmt.Errorf("HAProxy returned an error from %s: %s", addr, trimmedResponse)
	}

	return nil // Success
}

// BlockIPForDuration sends a block command to ALL configured HAProxy instances concurrently.
// It uses a best-effort approach and will always return nil (success) to the caller,
// ensuring the main log processing is not interrupted by HAProxy failures.
func BlockIPForDuration(ip string, duration time.Duration) error {
	if DryRun {
		LogOutput(LevelInfo, "DRYRUN", "Would block IP %s for %v (Expiry is HAProxy config-driven).", ip, duration)
		return nil
	}

	// 1. Determine the stick table name based on the requested duration
	DurationTableMutex.RLock()
	tableName, exists := DurationToTableName[duration]
	fallbackName := BlockTableNameFallback
	DurationTableMutex.RUnlock()

	// Check if the exact duration exists
	if !exists {
		tableName = fallbackName

		// If the fallback is also empty (no tables configured), skip with warning
		if tableName == "" {
			LogOutput(LevelWarning, "SKIP_BLOCK", "No HAProxy duration tables configured. Skipping block attempt for IP %s.", ip)
			return nil
		}
		LogOutput(LevelWarning, "WARN", "Requested block duration %v has no specific table. Falling back to %s table.", duration, tableName)
	}

	// 2. Construct the command (using stick table)
	// We use 'set table' as it respects the stick table's configured 'expire' time.
	command := fmt.Sprintf("set table %s key %s data 1\n", tableName, ip)

	// 3. Get the list of HAProxy addresses from the global state (protected by RLock)
	HAProxyMutex.RLock()
	addresses := HAProxyAddresses
	HAProxyMutex.RUnlock()

	if len(addresses) == 0 {
		// Log a warning and return nil (success, no blocking action taken)
		LogOutput(LevelWarning, "SKIP_BLOCK", "HAProxy addresses list is empty. Skipping block attempt for IP %s.", ip)
		return nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(addresses))

	// 4. Concurrently execute block on all instances
	for _, addr := range addresses {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()

			if err := blockIPOnInstance(addr, ip, command); err != nil {
				// Log the error immediately at LevelError
				LogOutput(LevelError, "HAPROXY_FAIL", "HAProxy block failed on instance %s for IP %s: %v", addr, ip, err)
				errs <- err
			} else {
				LogOutput(LevelInfo, "HAPROXY_BLOCK", "Successfully blocked IP %s on instance %s in table %s.", ip, addr, tableName)
			}
		}(addr)
	}

	wg.Wait()
	close(errs)

	// 5. Final Error Check (Logging only, always return nil for graceful failure)
	if len(errs) > 0 {
		// Log a summary warning that the operation failed on some instances.
		LogOutput(LevelWarning, "FAILSAFE", "Block for IP %s partially failed on %d/%d instances. Check HAPROXY_FAIL logs for details.", ip, len(errs), len(addresses))
	}

	// CRITICAL: Always return nil to ensure the main program loop is NOT interrupted.
	return nil
}
