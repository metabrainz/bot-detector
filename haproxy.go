package main

import (
	"bot-detector/internal/logging"
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CommandExecutor defines the function signature for executing a single backend command.
// This allows the real network logic to be easily mocked for unit testing.
type CommandExecutor func(addr, ip, command string) error

// HAProxyBlocker is a concrete implementation of the Blocker interface that interacts with HAProxy.
type HAProxyBlocker struct {
	// A reference to the main processor to access config, logging, and the executor.
	P *Processor
}

// Block adds an IP to the appropriate HAProxy stick table.
func (b *HAProxyBlocker) Block(ipInfo IPInfo, duration time.Duration) error {
	p := b.P
	if p.DryRun {
		p.LogFunc(logging.LevelInfo, "DRY_RUN", "Would block IP %s for %v (Chain complete).", ipInfo.Address, duration)
		return nil
	}

	// 1. Determine table name for the given duration (using DurationToTableName)
	p.ConfigMutex.RLock()
	baseTableName, found := p.Config.DurationToTableName[duration]
	if !found {
		// If duration not found, use the fallback table
		baseTableName = p.Config.BlockTableNameFallback
	}
	p.ConfigMutex.RUnlock()

	if baseTableName == "" {
		p.LogFunc(logging.LevelWarning, "SKIP_BLOCK", "No HAProxy table found for block duration %v. Skipping block attempt for IP %s.", duration, ipInfo.Address)
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
		p.LogFunc(logging.LevelError, "SKIP_BLOCK", "cannot block IP %s: invalid IP version", ipInfo.Address)
		return nil
	}

	// Command to block an IP: set table <table> key <key> data.gpc0 1
	command := fmt.Sprintf("set table %s key %s data.gpc0 1\n", tableName, ipInfo.Address)

	// 3. Construct the targets map for concurrent execution
	targets := make(map[string]map[string]string)
	targets[tableName] = make(map[string]string)

	addresses := p.Config.BlockerAddresses

	for _, addr := range addresses {
		targets[tableName][addr] = command
	}

	// 4. Execute concurrently
	return b.executeCommandsConcurrently(ipInfo.Address, targets)
}

// Unblock removes an IP from all configured HAProxy stick tables.
func (b *HAProxyBlocker) Unblock(ipInfo IPInfo) error {
	p := b.P
	if p.DryRun {
		p.LogFunc(logging.LevelInfo, "DRY_RUN", "Would unblock IP %s from all tables/maps.", ipInfo.Address)
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
		p.LogFunc(logging.LevelError, "SKIP_UNBLOCK", "Cannot unblock IP %s: unrecognized IP version", ipInfo.Address)
		return nil
	}

	// 1. Determine all tables/maps to delete from
	p.ConfigMutex.RLock()
	baseTables := make(map[string]struct{})
	for _, baseName := range p.Config.DurationToTableName {
		baseTables[baseName] = struct{}{}
	}
	if p.Config.BlockTableNameFallback != "" {
		baseTables[p.Config.BlockTableNameFallback] = struct{}{}
	}

	// 2. Construct the commands and the targets map
	targets := make(map[string]map[string]string)

	addresses := p.Config.BlockerAddresses
	p.ConfigMutex.RUnlock()

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
	return b.executeCommandsConcurrently(ipInfo.Address, targets)
}

// executeCommandImpl connects to a single HAProxy instance over TCP/Unix and executes the command.
// This contains the original networking logic.
func executeCommandImpl(p *Processor, addr, ip, command string) error {
	// Determine network type: "unix" for local socket, "tcp" otherwise
	network := "tcp"
	if strings.Contains(addr, "/") { // Simple check for a file path
		network = "unix"
	}

	var lastErr error
	for attempt := 0; attempt < p.Config.BlockerMaxRetries; attempt++ {
		if attempt > 0 {
			// Log the retry attempt (assuming a LogOutput function is available)
			p.Metrics.BlockerRetries.Add(1)
			p.LogFunc(logging.LevelWarning, "HAPROXY_RETRY", "Retrying HAProxy command for %s (Attempt %d/%d)", addr, attempt+1, p.Config.BlockerMaxRetries)
			time.Sleep(p.Config.BlockerRetryDelay)
		}

		// 1. Connection attempt
		conn, err := net.DialTimeout(network, addr, p.Config.BlockerDialTimeout)
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
		conn.SetReadDeadline(time.Now().Add(p.Config.BlockerDialTimeout))
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
			// Increment the counter for commands sent to this specific blocker address.
			if counter, ok := p.Metrics.CmdsPerBlocker.Load(addr); ok {
				if c, ok := counter.(*atomic.Int64); ok {
					c.Add(1)
				}
			}
			return nil
		}

		// Any other read error (timeout, broken pipe, etc.) -> retry
		lastErr = fmt.Errorf("HAProxy response read error from %s: %w", addr, err)
	}

	// If the loop finishes without success, return the last collected error.
	return lastErr
}

// executeHAProxyCommandsConcurrently handles the concurrent execution of multiple commands
// against HAProxy instances.
func (b *HAProxyBlocker) executeCommandsConcurrently(ip string, targets map[string]map[string]string) error {
	p := b.P
	addresses := p.Config.BlockerAddresses

	if len(addresses) == 0 {
		p.LogFunc(logging.LevelWarning, "SKIP_COMMAND", "HAProxy addresses list is empty. Skipping command for IP %s.", ip)
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

				if err := p.CommandExecutor(p, addr, ip, command); err != nil {
					errs <- err
					// Log the error immediately at LevelError
					p.LogFunc(logging.LevelError, "HAPROXY_FAIL", "HAProxy command failed on instance %s for IP %s (Table %s): %v", addr, ip, tableName, err)
				} else {
					p.LogFunc(logging.LevelInfo, "HAPROXY_SUCCESS", "Successfully sent command for IP %s on instance %s to table %s.", ip, addr, tableName)
				}
			}(addr, command)
		}
	}

	wg.Wait()
	close(errs)

	// Final Error Check.
	// If any of the commands failed, we log a warning and return an aggregated error.
	// This ensures that partial failures (e.g., one HAProxy instance being down)
	// are not silently ignored by the caller.
	if numErrs := len(errs); numErrs > 0 {
		p.LogFunc(logging.LevelWarning, "HAPROXY_WARN", "One or more HAProxy instances failed to process command for IP %s. Total failures: %d", ip, numErrs)
		return fmt.Errorf("%d HAProxy commands failed for IP %s", numErrs, ip)
	}

	return nil
}
