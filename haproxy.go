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

// executeHAProxyCommand connects to a single HAProxy instance over TCP and executes the command.
// This function remains the low-level communication layer.
func executeHAProxyCommand(addr, ip, command string) error {
	// Use a short timeout to prevent connection hangs
	const dialTimeout = 5 * time.Second

	// 1. Connection attempt
	// Determine network type: "unix" for local socket, "tcp" otherwise
	network := "tcp"
	if strings.Contains(addr, "/") { // Simple check for a file path
		network = "unix"
	}

	// 1. Connection attempt
	conn, err := net.DialTimeout(network, addr, dialTimeout)
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
		// HAProxy returned a non-empty string, indicating a command syntax or
		// execution error (e.g., "No such table").
		return fmt.Errorf("HAProxy command execution failed for IP %s (Response: %s)", ip, trimmedResponse)
	}

	return nil
}

// executeHAProxyCommandsConcurrently handles the concurrent execution of multiple commands
// (map[table_name]map[haproxy_addr]command) against HAProxy instances.
// This abstracts away the concurrency and error reporting logic.
func executeHAProxyCommandsConcurrently(ip string, targets map[string]map[string]string) {
	HAProxyMutex.RLock()
	addresses := HAProxyAddresses
	HAProxyMutex.RUnlock()

	if len(addresses) == 0 {
		LogOutput(LevelWarning, "SKIP_COMMAND", "HAProxy addresses list is empty. Skipping command for IP %s.", ip)
		return
	}

	// Calculate total number of goroutines required
	totalGoroutines := 0
	for tableName := range targets {
		totalGoroutines += len(targets[tableName])
	}

	if totalGoroutines == 0 {
		return // Nothing to execute
	}

	var wg sync.WaitGroup
	errs := make(chan error, totalGoroutines)

	for tableName, commandsByAddr := range targets {
		// Capture current table name for the goroutine
		tableName := tableName
		for addr, command := range commandsByAddr {
			// Capture current address and command for the goroutine
			addr := addr
			command := command

			wg.Add(1)
			go func(addr, command string) {
				defer wg.Done()

				if err := executeHAProxyCommand(addr, ip, command); err != nil {
					errs <- err
					// Log the error immediately at LevelError
					LogOutput(LevelError, "HAPROXY_FAIL", "HAProxy command failed on instance %s for IP %s (Table %s): %v", addr, ip, tableName, err)
				} else {
					LogOutput(LevelInfo, "HAPROXY_SUCCESS", "Successfully sent command for IP %s on instance %s to table %s.", ip, addr, tableName)
				}
			}(addr, command)
		}
	}

	wg.Wait()
	close(errs)

	// Final Error Check (Logging only)
	if len(errs) > 0 {
		LogOutput(LevelWarning, "HAPROXY_WARN", "One or more HAProxy instances failed to process command for IP %s. Total failures: %d", ip, len(errs))
	}
}

// BlockIP blocks an IP address across all configured HAProxy instances and tables.
// This function is required by the tests and the refactored checker.go.
func BlockIP(ip string, duration time.Duration) error {
	if DryRun {
		LogOutput(LevelInfo, "DRYRUN", "Would block IP %s for %v (Chain complete).", ip, duration)
		return nil
	}

	// 1. Determine table name for the given duration (using DurationToTableName)
	DurationTableMutex.RLock()
	tableName, found := DurationToTableName[duration]
	if !found {
		tableName = BlockTableNameFallback
	}
	DurationTableMutex.RUnlock()

	if tableName == "" {
		LogOutput(LevelWarning, "SKIP_BLOCK", "No HAProxy table found for block duration %v. Skipping block attempt for IP %s.", duration, ip)
		return nil
	}

	// Command to add/update a stick table entry: set table <table> key <key> data 1
	command := fmt.Sprintf("set table %s key %s data 1\n", tableName, ip)

	// 2. Construct the targets map and execute concurrently
	targets := make(map[string]map[string]string)
	targets[tableName] = make(map[string]string)

	HAProxyMutex.RLock()
	addresses := HAProxyAddresses
	HAProxyMutex.RUnlock()

	for _, addr := range addresses {
		targets[tableName][addr] = command
	}

	// 3. Execute concurrently
	executeHAProxyCommandsConcurrently(ip, targets)

	return nil // Error logging is handled inside the concurrent executor
}

// UnblockIP removes an IP from all configured HAProxy stick tables/maps.
// This is primarily used when an IP is added to the whitelist and should no longer be blocked.
func UnblockIP(ip string) error {
	if DryRun {
		LogOutput(LevelInfo, "DRYRUN", "Would unblock IP %s from all tables/maps.", ip)
		return nil
	}

	// 1. Determine all tables/maps to delete from
	DurationTableMutex.RLock()
	tables := make(map[string]struct{})
	for _, tableName := range DurationToTableName {
		tables[tableName] = struct{}{}
	}
	if BlockTableNameFallback != "" {
		tables[BlockTableNameFallback] = struct{}{}
	}
	DurationTableMutex.RUnlock()

	// 2. Construct the commands and the targets map
	targets := make(map[string]map[string]string)

	HAProxyMutex.RLock()
	addresses := HAProxyAddresses
	HAProxyMutex.RUnlock()

	for tableName := range tables {
		targets[tableName] = make(map[string]string)

		// Command to remove an entry from a stick table: set table <table> key <key> remove
		command := fmt.Sprintf("set table %s key %s remove\n", tableName, ip)

		for _, addr := range addresses {
			targets[tableName][addr] = command
		}
	}

	// 3. Execute concurrently
	executeHAProxyCommandsConcurrently(ip, targets)

	return nil // Error logging is handled inside the concurrent executor
}
