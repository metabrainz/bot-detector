package blocker

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/utils" // Added for IPInfo
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp" // Added for parsing HAProxy table entries
	"sort"   // Added for sorting backend addresses
	"strings"
	"sync"
	"time"
)

// Regex to parse a single line from "show table <name>" output.
// Example: "0x564f26146268: key=1.10.230.15 use=0 exp=51153745 gpc0=1"
var haProxyTableEntryRegex = regexp.MustCompile(`^[^:]+:\s+key=(?P<ip>\S+)\s+use=\d+\s+exp=(?P<exp>\d+)\s+gpc0=(?P<gpc0>\d+)`)

// HAProxyTableEntry represents a single entry in an HAProxy stick table.
type HAProxyTableEntry struct {
	IP      string
	Exp     int64  // Remaining milliseconds until expiration
	Gpc0    int    // General purpose counter, 1 for blocked
	RawLine string // Store the original line for detailed output
}

// SyncDiscrepancy reports a synchronization difference between two HAProxy backends.
type SyncDiscrepancy struct {
	IP          string
	TableName   string
	BackendA    string
	BackendB    string
	EntryA      *HAProxyTableEntry // nil if missing in A
	EntryB      *HAProxyTableEntry // nil if missing in B
	Reason      string             // e.g., "Missing in BackendB", "Gpc0 Mismatch", "Expiration Mismatch"
	DiffMillis  int64              // Only for expiration mismatches, absolute difference in milliseconds
}

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
				entriesInTable, err := b.getHAProxyIPsInTable(currentAddr, tableName)
				if err != nil {
					errs <- fmt.Errorf("failed to get IPs from table %s on %s: %w", tableName, currentAddr, err)
					return
				}

				mu.Lock()
				for _, entry := range entriesInTable {
					allBlockedIPs[entry.IP] = struct{}{}
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

// CompareHAProxyBackends compares the stick table entries across multiple HAProxy backends
// and reports any synchronization discrepancies.
func (b *HAProxyBlocker) CompareHAProxyBackends(expTolerance time.Duration) ([]SyncDiscrepancy, error) {
	addresses := b.P.GetBlockerAddresses()
	if len(addresses) < 2 {
		return nil, fmt.Errorf("at least two HAProxy addresses are required for comparison")
	}

	// Map to store all entries from all backends: backend_addr -> table_name -> ip_addr -> HAProxyTableEntry
	allBackendEntries := make(map[string]map[string]map[string]HAProxyTableEntry)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex // Protects allBackendEntries and errors
		errs []error
	)

	// Concurrently fetch all entries from all backends
	for _, addr := range addresses {
		wg.Add(1)
		go func(currentAddr string) {
			defer wg.Done()

			backendEntries := make(map[string]map[string]HAProxyTableEntry)
			tableNames, err := b.getHAProxyTableNames(currentAddr)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("failed to get table names from %s: %w", currentAddr, err))
				mu.Unlock()
				return
			}

			for _, tableName := range tableNames {
				entries, err := b.getHAProxyIPsInTable(currentAddr, tableName)
				if err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("failed to get entries from table %s on %s: %w", tableName, currentAddr, err))
					mu.Unlock()
					return
				}
				if len(entries) > 0 {
					if _, ok := backendEntries[tableName]; !ok {
						backendEntries[tableName] = make(map[string]HAProxyTableEntry)
					}
					for _, entry := range entries {
						backendEntries[tableName][entry.IP] = entry
					}
				}
			}

			mu.Lock()
			allBackendEntries[currentAddr] = backendEntries
			mu.Unlock()
		}(addr)
	}
	wg.Wait()

	if len(errs) > 0 {
		return nil, fmt.Errorf("errors occurred during data collection: %v", errs)
	}

	var discrepancies []SyncDiscrepancy

	// Compare entries across backends
	// We'll use the first backend as the reference, but check all pairs.
	// This ensures we catch discrepancies even if the first backend is missing an entry.
	backendAddrs := make([]string, 0, len(addresses))
	for addr := range allBackendEntries {
		backendAddrs = append(backendAddrs, addr)
	}
	sort.Strings(backendAddrs) // Ensure consistent order for comparison

	if len(backendAddrs) < 2 {
		return nil, fmt.Errorf("not enough backends with data to compare")
	}

	// Iterate through all unique IPs and table names found across all backends
	uniqueIPsByTable := make(map[string]map[string]struct{}) // table_name -> ip_addr -> exists
	for _, backendAddr := range backendAddrs {
		for tableName, ipEntries := range allBackendEntries[backendAddr] {
			if _, ok := uniqueIPsByTable[tableName]; !ok {
				uniqueIPsByTable[tableName] = make(map[string]struct{})
			}
			for ip := range ipEntries {
				uniqueIPsByTable[tableName][ip] = struct{}{}
			}
		}
	}

	for tableName, ips := range uniqueIPsByTable {
		for ip := range ips {
			// Collect entry for this IP in this table from all backends
			entriesForIP := make(map[string]*HAProxyTableEntry) // backend_addr -> entry
			for _, backendAddr := range backendAddrs {
				if allBackendEntries[backendAddr][tableName] != nil {
					entry, found := allBackendEntries[backendAddr][tableName][ip]
					if found {
						entriesForIP[backendAddr] = &entry
					} else {
						entriesForIP[backendAddr] = nil // Explicitly mark as missing
					}
				} else {
					entriesForIP[backendAddr] = nil // Table not found in this backend
				}
			}

			// Now compare entriesForIP
			// Simple pairwise comparison for now, can be optimized for N backends
			for i := 0; i < len(backendAddrs); i++ {
				for j := i + 1; j < len(backendAddrs); j++ {
					addrA := backendAddrs[i]
					addrB := backendAddrs[j]
					entryA := entriesForIP[addrA]
					entryB := entriesForIP[addrB]

					if entryA == nil && entryB == nil {
						continue // Both missing, no discrepancy
					}

					if entryA == nil && entryB != nil {
						discrepancies = append(discrepancies, SyncDiscrepancy{
							IP:        ip,
							TableName: tableName,
							BackendA:  addrA,
							BackendB:  addrB,
							EntryA:    nil,
							EntryB:    entryB,
							Reason:    fmt.Sprintf("Missing in %s", addrA),
						})
						continue
					}

					if entryA != nil && entryB == nil {
						discrepancies = append(discrepancies, SyncDiscrepancy{
							IP:        ip,
							TableName: tableName,
							BackendA:  addrA,
							BackendB:  addrB,
							EntryA:    entryA,
							EntryB:    nil,
							Reason:    fmt.Sprintf("Missing in %s", addrB),
						})
						continue
					}

					// Both entries exist, compare gpc0 and exp
					if entryA.Gpc0 != entryB.Gpc0 {
						discrepancies = append(discrepancies, SyncDiscrepancy{
							IP:        ip,
							TableName: tableName,
							BackendA:  addrA,
							BackendB:  addrB,
							EntryA:    entryA,
							EntryB:    entryB,
							Reason:    "Gpc0 Mismatch",
						})
						continue
					}

					// Compare expiration with tolerance
					diffExp := entryA.Exp - entryB.Exp
					if diffExp < 0 {
						diffExp = -diffExp // Absolute difference
					}

					if time.Duration(diffExp)*time.Millisecond > expTolerance {
						discrepancies = append(discrepancies, SyncDiscrepancy{
							IP:          ip,
							TableName:   tableName,
							BackendA:    addrA,
							BackendB:    addrB,
							EntryA:      entryA,
							EntryB:      entryB,
							Reason:      "Expiration Mismatch",
							DiffMillis:  diffExp,
						})
						continue
					}
				}
			}
		}
	}

	return discrepancies, nil
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
func (b *HAProxyBlocker) getHAProxyIPsInTable(addr, tableName string) ([]HAProxyTableEntry, error) {
	command := fmt.Sprintf("show table %s\n", tableName)
	response, err := b.executeHAProxyCommand(addr, command)
	if err != nil {
		return nil, err
	}

	var entries []HAProxyTableEntry
	scanner := bufio.NewScanner(strings.NewReader(response))
	for scanner.Scan() {
		line := scanner.Text()
		match := haProxyTableEntryRegex.FindStringSubmatch(line)
		if match != nil {
			ip := match[haProxyTableEntryRegex.SubexpIndex("ip")]
			expStr := match[haProxyTableEntryRegex.SubexpIndex("exp")]
			gpc0Str := match[haProxyTableEntryRegex.SubexpIndex("gpc0")]

			exp, parseErr := utils.ParseInt64(expStr)
			if parseErr != nil {
				b.P.Log(logging.LevelError, "HAPROXY_PARSE_ERROR", "Failed to parse exp value '%s' for IP '%s' in table '%s' on '%s': %v", expStr, ip, tableName, addr, parseErr)
				continue
			}
			gpc0, parseErr := utils.ParseInt(gpc0Str)
			if parseErr != nil {
				b.P.Log(logging.LevelError, "HAPROXY_PARSE_ERROR", "Failed to parse gpc0 value '%s' for IP '%s' in table '%s' on '%s': %v", gpc0Str, ip, tableName, addr, parseErr)
				continue
			}

			// Only include entries that are actually blocked (gpc0=1)
			if gpc0 == 1 {
				entries = append(entries, HAProxyTableEntry{
					IP:      ip,
					Exp:     exp,
					Gpc0:    gpc0,
					RawLine: line,
				})
			}
		}
	}
	return entries, scanner.Err()
}
