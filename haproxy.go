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

// CommandExecutor defines the function signature for executing a single backend command.
// This allows the real network logic to be easily mocked for unit testing.
type CommandExecutor func(addr, ip, command string) error

// These variables define the retry and timeout behavior for HAProxy commands.
// They are package-level so they can be overridden in tests.
var (
	maxRetries  = 3
	retryDelay  = 200 * time.Millisecond
	dialTimeout = 5 * time.Second
)

// executeCommandImpl connects to a single HAProxy instance over TCP/Unix and executes the command.
// This contains the original networking logic.
func executeCommandImpl(addr, ip, command string) error {
	// Determine network type: "unix" for local socket, "tcp" otherwise
	network := "tcp"
	if strings.Contains(addr, "/") { // Simple check for a file path
		network = "unix"
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Log the retry attempt (assuming a LogOutput function is available)
			LogOutput(LevelWarning, "HAPROXY_RETRY", "Retrying HAProxy command for %s (Attempt %d/%d)", addr, attempt+1, maxRetries)
			time.Sleep(retryDelay)
		}

		// 1. Connection attempt
		conn, err := net.DialTimeout(network, addr, dialTimeout)
		if err != nil {
			lastErr = fmt.Errorf("failed to connect to HAProxy instance %s: %w", addr, err)
			continue // Try again
		}

		// Close the connection in a deferred statement
		defer conn.Close()

		// 2. Write attempt
		if _, err = conn.Write([]byte(command)); err != nil {
			lastErr = fmt.Errorf("failed to send command to HAProxy instance %s: %w", addr, err)
			continue // Try again
		}

		// 3. Read attempt (Command Error Check)
		reader := bufio.NewReader(conn)
		conn.SetReadDeadline(time.Now().Add(dialTimeout))
		response, err := reader.ReadString('\n')

		// If the error is EOF or nil, the command might have succeeded.
		// FIX: Only treat io.EOF as success if we actually read some data (`response != ""`).
		// An io.EOF on an empty response means the connection closed abruptly, which is a failure.
		if err == nil || (errors.Is(err, io.EOF) && response != "") {
			trimmedResponse := strings.TrimSpace(response)

			if trimmedResponse != "" {
				// HAProxy returned a non-empty string, indicating a definitive
				// command execution error (e.g., "No such table").
				return fmt.Errorf("HAProxy command execution failed for IP %s (Response: %s)", ip, trimmedResponse)
			}

			// Success
			return nil
		}

		// Any other read error (timeout, broken pipe, etc.) -> retry
		lastErr = fmt.Errorf("HAProxy response read error from %s: %w", addr, err)
	}

	// If the loop finishes without success, return the last collected error.
	return lastErr
}

// executeHAProxyCommandsConcurrently handles the concurrent execution of multiple commands
// (map[table_name]map[haproxy_addr]command) against HAProxy instances.
func (p *Processor) executeHAProxyCommandsConcurrently(ip string, targets map[string]map[string]string) error {
	addresses := p.Config.HAProxyAddresses

	if len(addresses) == 0 {
		p.LogFunc(LevelWarning, "SKIP_COMMAND", "HAProxy addresses list is empty. Skipping command for IP %s.", ip)
		return nil
	}

	// Calculate total number of goroutines required
	totalGoroutines := 0
	for tableName := range targets {
		totalGoroutines += len(targets[tableName])
	}

	if totalGoroutines == 0 {
		return nil // Nothing to execute
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

				if err := p.CommandExecutor(addr, ip, command); err != nil {
					errs <- err
					// Log the error immediately at LevelError
					p.LogFunc(LevelError, "HAPROXY_FAIL", "HAProxy command failed on instance %s for IP %s (Table %s): %v", addr, ip, tableName, err)
				} else {
					p.LogFunc(LevelInfo, "HAPROXY_SUCCESS", "Successfully sent command for IP %s on instance %s to table %s.", ip, addr, tableName)
				}
			}(addr, command)
		}
	}

	wg.Wait()
	close(errs)

	// Final Error Check (Logging only)
	if numErrs := len(errs); numErrs > 0 {
		p.LogFunc(LevelWarning, "HAPROXY_WARN", "One or more HAProxy instances failed to process command for IP %s. Total failures: %d", ip, len(errs))
		return fmt.Errorf("%d HAProxy commands failed for IP %s", numErrs, ip)
	}
	return nil
}

// BlockIP adds an IP to the appropriate HAProxy stick table/map with a key set to '1' (blocked).
func (p *Processor) BlockIP(ipInfo IPInfo, duration time.Duration) error {
	if p.DryRun {
		p.LogFunc(LevelInfo, "DRYRUN", "Would block IP %s for %v (Chain complete).", ipInfo.Address, duration)
		return nil
	}

	// 1. Determine table name for the given duration (using DurationToTableName)
	p.ChainMutex.RLock()
	baseTableName, found := p.Config.DurationToTableName[duration]
	if !found {
		// If duration not found, use the fallback table
		baseTableName = p.Config.BlockTableNameFallback
	}
	p.ChainMutex.RUnlock()

	if baseTableName == "" {
		p.LogFunc(LevelWarning, "SKIP_BLOCK", "No HAProxy table found for block duration %v. Skipping block attempt for IP %s.", duration, ipInfo.Address)
		return nil
	}

	// 2. Determine the IP version suffix and handle invalid version
	tableName := baseTableName
	switch ipInfo.Version {
	case VersionIPv4:
		tableName += "_ipv4" // Simple string concatenation
	case VersionIPv6:
		tableName += "_ipv6" // Simple string concatenation
	default:
		p.LogFunc(LevelError, "SKIP_BLOCK", "cannot block IP %s: invalid IP version", ipInfo.Address)
		return nil
	}

	// Command to block an IP: set table <table> key <key> data.gpc0 1
	command := fmt.Sprintf("set table %s key %s data.gpc0 1\n", tableName, ipInfo.Address)

	// 3. Construct the targets map for concurrent execution
	targets := make(map[string]map[string]string)
	targets[tableName] = make(map[string]string)

	addresses := p.Config.HAProxyAddresses

	for _, addr := range addresses {
		targets[tableName][addr] = command
	}

	// 4. Execute concurrently
	return p.executeHAProxyCommandsConcurrently(ipInfo.Address, targets)
}

// UnblockIP removes an IP from all configured HAProxy stick tables/maps.
// This is primarily used when an IP is added to the whitelist and should no longer be blocked.
func (p *Processor) UnblockIP(ipInfo IPInfo) error {
	if p.DryRun {
		p.LogFunc(LevelInfo, "DRYRUN", "Would unblock IP %s from all tables/maps.", ipInfo.Address)
		return nil
	}

	var ipSuffix string
	switch ipInfo.Version {
	case VersionIPv4:
		ipSuffix = "_ipv4"
	case VersionIPv6:
		ipSuffix = "_ipv6"
	default:
		// If the IP is invalid or unrecognized, we cannot determine which table to clear,
		// so we skip the action.
		p.LogFunc(LevelError, "SKIP_UNBLOCK", "Cannot unblock IP %s: unrecognized IP version", ipInfo.Address)
		return nil
	}

	// 1. Determine all BASE table names to clear from
	// 1. Determine all tables/maps to delete from
	p.ChainMutex.RLock()
	baseTables := make(map[string]struct{})
	for _, baseName := range p.Config.DurationToTableName {
		baseTables[baseName] = struct{}{}
	}
	if p.Config.BlockTableNameFallback != "" {
		baseTables[p.Config.BlockTableNameFallback] = struct{}{}
	}

	// 2. Construct the commands and the targets map
	targets := make(map[string]map[string]string)

	addresses := p.Config.HAProxyAddresses
	p.ChainMutex.RUnlock()

	// We only clear the tables matching the IP's version
	for baseName := range baseTables {
		// Construct the full, version-dependent table name
		tableName := baseName + ipSuffix
		targets[tableName] = make(map[string]string)

		// Command to remove an entry from a stick table: clear table <table> key <key>
		command := fmt.Sprintf("clear table %s key %s\n", tableName, ipInfo.Address)

		for _, addr := range addresses {
			targets[tableName][addr] = command
		}
	}

	// 3. Execute concurrently
	return p.executeHAProxyCommandsConcurrently(ipInfo.Address, targets)
}
