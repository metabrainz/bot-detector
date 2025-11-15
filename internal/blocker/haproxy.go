package blocker

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/utils" // Added for IPInfo
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// HAProxyProvider defines the interface the HAProxy blocker needs from its owner.
type HAProxyProvider interface {
	LogProvider
	GetBlockerAddresses() []string
	GetDurationTables() map[time.Duration]string
	GetBlockTableNameFallback() string
	GetBlockerMaxRetries() int
	GetBlockerRetryDelay() time.Duration
	GetBlockerDialTimeout() time.Duration
	IncrementBlockerRetries()
	IncrementCmdsPerBlocker(addr string)
}

// CommandExecutor defines the function signature for executing a single backend command.
type CommandExecutor func(addr, ip, command string) error

// HAProxyBlocker is a concrete implementation of the Blocker interface that interacts with HAProxy.
type HAProxyBlocker struct {
	P        HAProxyProvider
	Executor CommandExecutor
	IsDryRun bool
}

// NewHAProxyBlocker creates a new HAProxyBlocker.
func NewHAProxyBlocker(p HAProxyProvider, dryRun bool) *HAProxyBlocker {
	b := &HAProxyBlocker{P: p, IsDryRun: dryRun}
	b.Executor = b.executeCommandImpl
	return b
}

// Block adds an IP to the appropriate HAProxy stick table.
func (b *HAProxyBlocker) Block(ipInfo utils.IPInfo, duration time.Duration) error {
	if b.IsDryRun {
		b.P.Log(logging.LevelInfo, "DRY_RUN", "Would block IP %s for %v (Chain complete).", ipInfo.Address, duration)
		return nil
	}

	baseTableName, found := b.P.GetDurationTables()[duration]
	if !found {
		baseTableName = b.P.GetBlockTableNameFallback()
	}

	if baseTableName == "" {
		b.P.Log(logging.LevelWarning, "SKIP_BLOCK", "No HAProxy table found for block duration %v. Skipping block attempt for IP %s.", duration, ipInfo.Address)
		return nil
	}

	tableName := baseTableName
	switch ipInfo.Version {
	case 4:
		tableName += "_ipv4"
	case 6:
		tableName += "_ipv6"
	default:
		b.P.Log(logging.LevelError, "SKIP_BLOCK", "cannot block IP %s: invalid IP version", ipInfo.Address)
		return nil
	}

	command := fmt.Sprintf("set table %s key %s data.gpc0 1\n", tableName, ipInfo.Address)
	targets := make(map[string]map[string]string)
	targets[tableName] = make(map[string]string)
	for _, addr := range b.P.GetBlockerAddresses() {
		targets[tableName][addr] = command
	}

	return b.executeCommandsConcurrently(ipInfo.Address, targets, "block")
}

// Unblock removes an IP from all configured HAProxy stick tables.
func (b *HAProxyBlocker) Unblock(ipInfo utils.IPInfo) error {
	if b.IsDryRun {
		b.P.Log(logging.LevelInfo, "DRY_RUN", "Would unblock IP %s from all tables/maps.", ipInfo.Address)
		return nil
	}

	var ipSuffix string
	switch ipInfo.Version {
	case 4:
		ipSuffix = "_ipv4"
	case 6:
		ipSuffix = "_ipv6"
	default:
		b.P.Log(logging.LevelError, "SKIP_UNBLOCK", "Cannot unblock IP %s: unrecognized IP version", ipInfo.Address)
		return nil
	}

	baseTables := make(map[string]struct{})
	for _, baseName := range b.P.GetDurationTables() {
		baseTables[baseName] = struct{}{}
	}
	if fallback := b.P.GetBlockTableNameFallback(); fallback != "" {
		baseTables[fallback] = struct{}{}
	}

	targets := make(map[string]map[string]string)
	for baseName := range baseTables {
		tableName := baseName + ipSuffix
		targets[tableName] = make(map[string]string)
		command := fmt.Sprintf("clear table %s key %s\n", tableName, ipInfo.Address)
		for _, addr := range b.P.GetBlockerAddresses() {
			targets[tableName][addr] = command
		}
	}

	return b.executeCommandsConcurrently(ipInfo.Address, targets, "unblock")
}

// executeCommandImpl connects to a single HAProxy instance and executes a command.
func (b *HAProxyBlocker) executeCommandImpl(addr, ip, command string) error {
	network := "tcp"
	if strings.Contains(addr, "/") {
		network = "unix"
	}

	var lastErr error
	for attempt := 0; attempt < b.P.GetBlockerMaxRetries(); attempt++ {
		if attempt > 0 {
			b.P.IncrementBlockerRetries()
			b.P.Log(logging.LevelWarning, "HAPROXY_RETRY", "Retrying HAProxy command for %s (Attempt %d/%d)", addr, attempt+1, b.P.GetBlockerMaxRetries())
			time.Sleep(b.P.GetBlockerRetryDelay())
		}

		conn, err := net.DialTimeout(network, addr, b.P.GetBlockerDialTimeout())
		if err != nil {
			lastErr = fmt.Errorf("failed to connect to HAProxy instance %s: %w", addr, err)
			continue
		}
		defer func() {
			_ = conn.Close()
		}()

		if _, err = conn.Write([]byte(command)); err != nil {
			lastErr = fmt.Errorf("failed to send command to HAProxy instance %s: %w", addr, err)
			continue
		}

		reader := bufio.NewReader(conn)
		_ = conn.SetReadDeadline(time.Now().Add(b.P.GetBlockerDialTimeout()))
		response, err := reader.ReadString('\n')

		if err == nil || (errors.Is(err, io.EOF) && response != "") {
			trimmedResponse := strings.TrimSpace(response)
			if trimmedResponse != "" {
				return fmt.Errorf("HAProxy command execution failed for IP %s (Response: %s)", ip, trimmedResponse)
			}
			b.P.IncrementCmdsPerBlocker(addr)
			return nil
		}
		lastErr = fmt.Errorf("HAProxy response read error from %s: %w", addr, err)
	}
	return lastErr
}

// executeCommandImpl connects to a single HAProxy instance and executes a command.
func (b *HAProxyBlocker) executeHAProxyCommand(addr, command string) (string, error) {
	network := "tcp"
	if strings.Contains(addr, "/") {
		network = "unix"
	}

	var lastErr error
	for attempt := 0; attempt < b.P.GetBlockerMaxRetries(); attempt++ {
		if attempt > 0 {
			b.P.IncrementBlockerRetries()
			b.P.Log(logging.LevelWarning, "HAPROXY_RETRY", "Retrying HAProxy command for %s (Attempt %d/%d)", addr, attempt+1, b.P.GetBlockerMaxRetries())
			time.Sleep(b.P.GetBlockerRetryDelay())
		}

		conn, err := net.DialTimeout(network, addr, b.P.GetBlockerDialTimeout())
		if err != nil {
			lastErr = fmt.Errorf("failed to connect to HAProxy instance %s: %w", addr, err)
			continue
		}
		defer func() {
			_ = conn.Close()
		}()

		if _, err = conn.Write([]byte(command)); err != nil {
			lastErr = fmt.Errorf("failed to send command to HAProxy instance %s: %w", addr, err)
			continue
		}

		reader := bufio.NewReader(conn)
		_ = conn.SetReadDeadline(time.Now().Add(b.P.GetBlockerDialTimeout()))
		response, err := reader.ReadString('\n')

		if err == nil || (errors.Is(err, io.EOF) && response != "") {
			trimmedResponse := strings.TrimSpace(response)
			if trimmedResponse == "200 OK" {
				b.P.IncrementCmdsPerBlocker(addr)
				return trimmedResponse, nil
			} else if strings.HasPrefix(trimmedResponse, "500") || strings.HasPrefix(trimmedResponse, "400") {
				return "", fmt.Errorf("HAProxy command execution failed on %s (Response: %s)", addr, trimmedResponse)
			}
			b.P.IncrementCmdsPerBlocker(addr)
			return trimmedResponse, nil
		}
		lastErr = fmt.Errorf("HAProxy response read error from %s: %w", addr, err)
	}
	return "", lastErr
}

// executeCommandsConcurrently handles the concurrent execution of multiple commands.
func (b *HAProxyBlocker) executeCommandsConcurrently(ip string, targets map[string]map[string]string, commandType string) error {
	addresses := b.P.GetBlockerAddresses()
	if len(addresses) == 0 {
		b.P.Log(logging.LevelWarning, "SKIP_COMMAND", "HAProxy addresses list is empty. Skipping '%s' command for IP %s.", commandType, ip)
		return nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(targets)*len(addresses))

	for tableName, commandsByAddr := range targets {
		for addr, command := range commandsByAddr {
			wg.Add(1)
			go func(a, c, tn string) {
				defer wg.Done()
				if err := b.Executor(a, ip, c); err != nil {
					errs <- err
					b.P.Log(logging.LevelError, "HAPROXY_FAIL", "HAProxy command failed on instance %s for IP %s (Table %s): %v", a, ip, tn, err)
				} else {
					b.P.Log(logging.LevelDebug, "HAPROXY_SUCCESS", "Successfully sent command for IP %s on instance %s to table %s.", ip, a, tn)
				}
			}(addr, command, tableName)
		}
	}

	wg.Wait()
	close(errs)

	if numErrs := len(errs); numErrs > 0 {
		b.P.Log(logging.LevelWarning, "HAPROXY_WARN", "One or more HAProxy instances failed to process command for IP %s. Total failures: %d", ip, numErrs)
		return fmt.Errorf("%d HAProxy '%s' commands failed for IP %s", numErrs, commandType, ip)
	}
	return nil
}

// ListBlocked retrieves all currently blocked IPs from HAProxy.
func (b *HAProxyBlocker) ListBlocked() ([]string, error) {
	if b.IsDryRun {
		b.P.Log(logging.LevelInfo, "DRY_RUN", "Would list blocked IPs.")
		return []string{}, nil
	}

	addresses := b.P.GetBlockerAddresses()
	if len(addresses) == 0 {
		b.P.Log(logging.LevelWarning, "SKIP_COMMAND", "HAProxy addresses list is empty. Skipping 'list blocked' command.")
		return nil, nil
	}

	var (
		allBlockedIPs = make(map[string]struct{})
		mu            sync.Mutex
		wg            sync.WaitGroup
		errs          = make(chan error, len(addresses))
	)

	for _, addr := range addresses {
		wg.Add(1)
		go func(currentAddr string) {
			defer wg.Done()

			// 1. Get all table names
			tableNames, err := b.getHAProxyTableNames(currentAddr)
			if err != nil {
				errs <- fmt.Errorf("failed to get table names from %s: %w", currentAddr, err)
				return
			}

			// 2. For each table, get blocked IPs
			for _, tableName := range tableNames {
				ipsInTable, err := b.getHAProxyIPsInTable(currentAddr, tableName)
				if err != nil {
					errs <- fmt.Errorf("failed to get IPs from table %s on %s: %w", tableName, currentAddr, err)
					return
				}

				mu.Lock()
				for _, ip := range ipsInTable {
					allBlockedIPs[ip] = struct{}{}
				}
				mu.Unlock()
			}
		}(addr)
	}

	wg.Wait()
	close(errs)

	if numErrs := len(errs); numErrs > 0 {
		b.P.Log(logging.LevelWarning, "HAPROXY_WARN", "One or more HAProxy instances failed to list blocked IPs. Total failures: %d", numErrs)
		// Collect all errors
		var errorMessages []string
		for err := range errs {
			errorMessages = append(errorMessages, err.Error())
		}
		return nil, fmt.Errorf("%d HAProxy 'list blocked' commands failed: %s", numErrs, strings.Join(errorMessages, "; "))
	}

	var result []string
	for ip := range allBlockedIPs {
		result = append(result, ip)
	}
	return result, nil
}

// getHAProxyTableNames executes "show table" and parses the response to get table names.
func (b *HAProxyBlocker) getHAProxyTableNames(addr string) ([]string, error) {
	command := "show table\n"
	response, err := b.executeHAProxyCommand(addr, command)
	if err != nil {
		return nil, err
	}

	var tableNames []string
	scanner := bufio.NewScanner(strings.NewReader(response))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "# table: ") {
			parts := strings.Split(line, ",")
			if len(parts) > 0 {
				tableNamePart := strings.TrimPrefix(parts[0], "# table: ")
				tableName := strings.TrimSpace(tableNamePart)
				tableNames = append(tableNames, tableName)
			}
		}
	}
	return tableNames, scanner.Err()
}

// getHAProxyIPsInTable executes "show table <name>" and parses the response to get IPs.
func (b *HAProxyBlocker) getHAProxyIPsInTable(addr, tableName string) ([]string, error) {
	command := fmt.Sprintf("show table %s\n", tableName)
	response, err := b.executeHAProxyCommand(addr, command)
	if err != nil {
		return nil, err
	}

	var ips []string
	scanner := bufio.NewScanner(strings.NewReader(response))
	for scanner.Scan() {
		line := scanner.Text()
		// Example line: "0x564f26146268: key=1.10.230.15 use=0 exp=51153745 gpc0=1"
		if strings.Contains(line, "key=") && strings.Contains(line, "gpc0=1") {
			parts := strings.Fields(line)
			for _, part := range parts {
				if strings.HasPrefix(part, "key=") {
					ip := strings.TrimPrefix(part, "key=")
					ips = append(ips, ip)
					break
				}
			}
		}
	}
	return ips, scanner.Err()
}
