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
var haProxyTableEntryRegex = regexp.MustCompile(`^\S+\s+key=(?P<ip>\S+)\s+use=\d+\s+exp=(?P<exp>\d+)\s+gpc0=(?P<gpc0>\d+)`)

// HAProxyTableEntry represents a single entry in an HAProxy stick table.
type HAProxyTableEntry struct {
	IP      string
	Exp     int64  // Remaining milliseconds until expiration
	Gpc0    int    // General purpose counter, 1 for blocked
	RawLine string // Store the original line for detailed output
}

// SyncDiscrepancy reports a synchronization difference between two HAProxy backends.
type SyncDiscrepancy struct {
	IP         string
	TableName  string
	BackendA   string
	BackendB   string
	EntryA     *HAProxyTableEntry // nil if missing in A
	EntryB     *HAProxyTableEntry // nil if missing in B
	Reason     string             // e.g., "Missing in BackendB", "Gpc0 Mismatch", "Expiration Mismatch"
	DiffMillis int64              // Only for expiration mismatches, absolute difference in milliseconds
	Details    map[string]string  // More granular details about the discrepancy
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
	P                         HAProxyProvider
	Executor                  CommandExecutor
	IsDryRun                  bool
	ExecuteHAProxyCommandFunc func(addr, command string) (string, error)
}

// NewHAProxyBlocker creates a new HAProxyBlocker.
func NewHAProxyBlocker(p HAProxyProvider, dryRun bool) *HAProxyBlocker {
	b := &HAProxyBlocker{P: p, IsDryRun: dryRun}
	b.Executor = b.executeCommandImpl
	b.ExecuteHAProxyCommandFunc = b.executeHAProxyCommand // Initialize the function field
	return b
}

// Block adds an IP to the appropriate HAProxy stick table.
func (b *HAProxyBlocker) Block(ipInfo utils.IPInfo, duration time.Duration, reason string) error {
	if b.IsDryRun {
		b.P.Log(logging.LevelInfo, "DRY_RUN", "Would block IP %s for %v (Reason: %s).", ipInfo.Address, duration, reason)
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
func (b *HAProxyBlocker) Unblock(ipInfo utils.IPInfo, reason string) error {
	if b.IsDryRun {
		b.P.Log(logging.LevelInfo, "DRY_RUN", "Would unblock IP %s from all tables/maps (Reason: %s).", ipInfo.Address, reason)
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
		command := fmt.Sprintf("set table %s key %s data.gpc0 0\n", tableName, ipInfo.Address)
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

	// Collect all unique table names and IPs across all backends
	uniqueTables := make(map[string]struct{})
	uniqueIPsByTable := make(map[string]map[string]struct{}) // table_name -> ip_addr -> exists

	for _, backendEntries := range allBackendEntries {
		for tableName, ipEntries := range backendEntries {
			if ipEntries == nil { // Add this check
				continue
			}
			uniqueTables[tableName] = struct{}{}
			if _, ok := uniqueIPsByTable[tableName]; !ok {
				uniqueIPsByTable[tableName] = make(map[string]struct{})
			}
			for ip := range ipEntries {
				uniqueIPsByTable[tableName][ip] = struct{}{}
			}
		}
	}

	// Ensure consistent order for backend addresses
	backendAddrs := make([]string, 0, len(addresses))
	for addr := range allBackendEntries {
		backendAddrs = append(backendAddrs, addr)
	}
	sort.Strings(backendAddrs)

	// Compare entries across all backends for each unique IP in each unique table
	for tableName := range uniqueTables {
		for ip := range uniqueIPsByTable[tableName] {
			// Collect all entries for this IP in this table across all backends
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

			// Now, compare the collected entries
			// We need to find if there's any discrepancy among the N backends
			// A discrepancy exists if:
			// 1. An IP is present in some backends but missing in others for the same table.
			// 2. An IP is present in multiple backends, but their gpc0 values differ.
			// 3. An IP is present in multiple backends, but their exp values differ beyond tolerance.

			// Check for presence/absence discrepancies
			presentBackends := []string{}
			missingBackends := []string{}
			for _, addr := range backendAddrs {
				if entriesForIP[addr] != nil {
					presentBackends = append(presentBackends, addr)
				} else {
					missingBackends = append(missingBackends, addr)
				}
			}

			if len(presentBackends) > 0 && len(missingBackends) > 0 {
				// IP is present in some, missing in others
				discrepancies = append(discrepancies, SyncDiscrepancy{
					IP:        ip,
					TableName: tableName,
					Reason:    "Presence Mismatch",
					Details: map[string]string{
						"present_in": strings.Join(presentBackends, ", "),
						"missing_in": strings.Join(missingBackends, ", "),
					},
				})
			}

			if len(presentBackends) > 1 {
				// Compare gpc0 and exp for present backends
				firstEntry := entriesForIP[presentBackends[0]]
				for i := 1; i < len(presentBackends); i++ {
					currentAddr := presentBackends[i]
					currentEntry := entriesForIP[currentAddr]

					// Gpc0 Mismatch
					if firstEntry.Gpc0 != currentEntry.Gpc0 {
						discrepancies = append(discrepancies, SyncDiscrepancy{
							IP:        ip,
							TableName: tableName,
							BackendA:  presentBackends[0],
							BackendB:  currentAddr,
							EntryA:    firstEntry,
							EntryB:    currentEntry,
							Reason:    "Gpc0 Mismatch",
							Details: map[string]string{
								fmt.Sprintf("gpc0_%s", presentBackends[0]): fmt.Sprintf("%d", firstEntry.Gpc0),
								fmt.Sprintf("gpc0_%s", currentAddr):        fmt.Sprintf("%d", currentEntry.Gpc0),
							},
						})
					}

					// Expiration Mismatch
					diffExp := firstEntry.Exp - currentEntry.Exp
					if diffExp < 0 {
						diffExp = -diffExp // Absolute difference
					}

					if time.Duration(diffExp)*time.Millisecond > expTolerance {
						discrepancies = append(discrepancies, SyncDiscrepancy{
							IP:         ip,
							TableName:  tableName,
							BackendA:   presentBackends[0],
							BackendB:   currentAddr,
							EntryA:     firstEntry,
							EntryB:     currentEntry,
							Reason:     "Expiration Mismatch",
							DiffMillis: diffExp,
							Details: map[string]string{
								fmt.Sprintf("exp_%s", presentBackends[0]): fmt.Sprintf("%d", firstEntry.Exp),
								fmt.Sprintf("exp_%s", currentAddr):        fmt.Sprintf("%d", currentEntry.Exp),
								"diff_millis":                             fmt.Sprintf("%d", diffExp),
								"tolerance_millis":                        fmt.Sprintf("%d", expTolerance.Milliseconds()),
							},
						})
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
	response, err := b.ExecuteHAProxyCommandFunc(addr, command)
	if err != nil {
		return nil, err
	}

	var tableNames []string
	scanner := bufio.NewScanner(strings.NewReader(response))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "table: ") {
			parts := strings.Split(line, ",")
			if len(parts) > 0 {
				tableNamePart := strings.TrimPrefix(parts[0], "table: ")
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
	response, err := b.ExecuteHAProxyCommandFunc(addr, command)
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

			entries = append(entries, HAProxyTableEntry{
				IP:      ip,
				Exp:     exp,
				Gpc0:    gpc0,
				RawLine: line,
			})
		}
	}
	return entries, scanner.Err()
}
