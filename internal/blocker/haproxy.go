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
var haProxyTableEntryRegex = regexp.MustCompile(`(?:key=(?P<ip>\S+))|(?:\s+exp=(?P<exp>\d+))|(?:\s+gpc0=(?P<gpc0>\d+))`)

// DefaultHAProxyBatchSize is the default number of commands to batch together with semicolons.
// HAProxy CLI supports multiple commands separated by semicolons in a single request.
// Tested successfully with 1000 commands (~52KB). Conservative default of 500.
// This can be overridden via the blockers.max_commands_per_batch configuration setting.
const DefaultHAProxyBatchSize = 500

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
	GetMaxCommandsPerBatch() int
	IncrementBlockerRetries()
	IncrementCmdsPerBlocker(addr string)
}

// CommandExecutor defines the function signature for executing a single backend command.
type CommandExecutor func(addr, ip, command string) error

// BackendHealth tracks the health state of a single HAProxy backend instance.
type BackendHealth struct {
	LastUptime  int64
	Healthy     bool
	LastCheck   time.Time
	NeedsResync bool
	mu          sync.RWMutex
}

// HAProxyBlocker is a concrete implementation of the Blocker interface that interacts with HAProxy.
type HAProxyBlocker struct {
	P                         HAProxyProvider
	Executor                  CommandExecutor
	IsDryRun                  bool
	ExecuteHAProxyCommandFunc func(addr, command string) (string, error)
	backendHealth             map[string]*BackendHealth
	healthMu                  sync.RWMutex
	healthCheckStop           chan struct{}
	healthCheckWg             sync.WaitGroup
}

// NewHAProxyBlocker creates a new HAProxyBlocker.
func NewHAProxyBlocker(p HAProxyProvider, dryRun bool) *HAProxyBlocker {
	b := &HAProxyBlocker{
		P:               p,
		IsDryRun:        dryRun,
		backendHealth:   make(map[string]*BackendHealth),
		healthCheckStop: make(chan struct{}),
	}
	b.Executor = b.executeCommandImpl
	b.ExecuteHAProxyCommandFunc = b.executeHAProxyCommand // Initialize the function field

	// Initialize health state for all backends
	for _, addr := range p.GetBlockerAddresses() {
		b.backendHealth[addr] = &BackendHealth{
			Healthy:   true,
			LastCheck: time.Now(),
		}
	}

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
	// Avoid double-suffixing if the user-provided table name already has one.
	if !strings.HasSuffix(baseTableName, "_ipv4") && !strings.HasSuffix(baseTableName, "_ipv6") {
		switch ipInfo.Version {
		case 4:
			tableName += "_ipv4"
		case 6:
			tableName += "_ipv6"
		default:
			b.P.Log(logging.LevelError, "SKIP_BLOCK", "cannot block IP %s: invalid IP version", ipInfo.Address)
			return nil
		}
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
		tableName := baseName
		if !strings.HasSuffix(baseName, "_ipv4") && !strings.HasSuffix(baseName, "_ipv6") {
			tableName += ipSuffix
		}
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
	cleanAddr := addr
	if strings.Contains(addr, "/") {
		network = "unix"
	} else if strings.HasPrefix(addr, "tcp:") {
		cleanAddr = strings.TrimPrefix(addr, "tcp:")
	}

	var lastErr error
	for attempt := 0; attempt < b.P.GetBlockerMaxRetries(); attempt++ {
		if attempt > 0 {
			b.P.IncrementBlockerRetries()
			b.P.Log(logging.LevelWarning, "HAPROXY_RETRY", "Retrying HAProxy command for %s (Attempt %d/%d)", addr, attempt+1, b.P.GetBlockerMaxRetries())
			time.Sleep(b.P.GetBlockerRetryDelay())
		}

		conn, err := net.DialTimeout(network, cleanAddr, b.P.GetBlockerDialTimeout())
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
	cleanAddr := addr
	if strings.Contains(addr, "/") {
		network = "unix"
	} else if strings.HasPrefix(addr, "tcp:") {
		cleanAddr = strings.TrimPrefix(addr, "tcp:")
	}

	var lastErr error
	for attempt := 0; attempt < b.P.GetBlockerMaxRetries(); attempt++ {
		if attempt > 0 {
			b.P.IncrementBlockerRetries()
			b.P.Log(logging.LevelWarning, "HAPROXY_RETRY", "Retrying HAProxy command for %s (Attempt %d/%d)", addr, attempt+1, b.P.GetBlockerMaxRetries())
			time.Sleep(b.P.GetBlockerRetryDelay())
		}

		conn, err := net.DialTimeout(network, cleanAddr, b.P.GetBlockerDialTimeout())
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
		response, err := io.ReadAll(reader)

		if err == nil || (errors.Is(err, io.EOF) && len(response) > 0) {
			responseStr := strings.TrimSpace(string(response))
			// Simple commands like "set table" don't have a response, so we can't check for "200 OK"
			// or other specific success messages. We assume success if there's no error.
			// For "show table", the response is the table content itself.
			b.P.IncrementCmdsPerBlocker(addr)
			return responseStr, nil
		}
		lastErr = fmt.Errorf("HAProxy response read error from %s: %w", addr, err)
	}
	return "", lastErr
}

// executeCommandsConcurrently handles the concurrent execution of multiple commands.
// For each address, it batches all commands together with semicolons for efficiency.
func (b *HAProxyBlocker) executeCommandsConcurrently(ip string, targets map[string]map[string]string, commandType string) error {

	addresses := b.P.GetBlockerAddresses()

	if len(addresses) == 0 {

		b.P.Log(logging.LevelWarning, "SKIP_COMMAND", "HAProxy addresses list is empty. Skipping '%s' command for IP %s.", commandType, ip)

		return nil

	}

	// Group commands by address for batching
	commandsByAddr := make(map[string][]string)
	for _, commandsMap := range targets {
		for addr, command := range commandsMap {
			commandsByAddr[addr] = append(commandsByAddr[addr], command)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(commandsByAddr))
	skippedCount := 0

	for addr, commands := range commandsByAddr {
		// Check if backend is healthy
		healthy, _, _ := b.GetBackendHealth(addr)
		if !healthy {
			skippedCount++
			b.P.Log(logging.LevelDebug, "SKIP_UNHEALTHY", "Skipping %s command for IP %s on unhealthy backend %s", commandType, ip, addr)
			continue
		}

		wg.Add(1)

		go func(a string, cmds []string) {
			defer wg.Done()

			// Batch commands with semicolons (HAProxy CLI supports this)
			batchedCommand := strings.Join(cmds, "; ")

			if err := b.Executor(a, ip, batchedCommand); err != nil {
				errs <- err
				b.P.Log(logging.LevelError, "HAPROXY_FAIL", "HAProxy batched command failed on instance %s for IP %s: %v", a, ip, err)
			} else {
				b.P.Log(logging.LevelDebug, "HAPROXY_SUCCESS", "Successfully sent %d batched commands for IP %s on instance %s.", len(cmds), ip, a)
			}

		}(addr, commands)
	}

	wg.Wait()

	close(errs)

	if numErrs := len(errs); numErrs > 0 {

		b.P.Log(logging.LevelWarning, "HAPROXY_WARN", "One or more HAProxy instances failed to process command for IP %s. Total failures: %d", ip, numErrs)

		return fmt.Errorf("%d HAProxy '%s' commands failed for IP %s", numErrs, commandType, ip)

	}

	if skippedCount > 0 && skippedCount == len(commandsByAddr) {
		b.P.Log(logging.LevelWarning, "ALL_BACKENDS_UNHEALTHY", "All backends are unhealthy. Command for IP %s queued but not executed.", ip)
	}

	return nil

}

// GetCurrentState retrieves current HAProxy state as a map of IP -> gpc0 value.
func (b *HAProxyBlocker) GetCurrentState() (map[string]int, error) {
	if b.IsDryRun {
		return make(map[string]int), nil
	}

	addresses := b.P.GetBlockerAddresses()
	if len(addresses) == 0 {
		return make(map[string]int), nil
	}

	var (
		state = make(map[string]int)
		mu    sync.Mutex
		wg    sync.WaitGroup
	)

	for _, addr := range addresses {
		wg.Add(1)
		go func(currentAddr string) {
			defer wg.Done()
			tableNames, err := b.getHAProxyTableNames(currentAddr)
			if err != nil {
				return
			}
			for _, tableName := range tableNames {
				entries, err := b.getHAProxyAllIPsInTable(currentAddr, tableName)
				if err != nil {
					continue
				}
				mu.Lock()
				for _, entry := range entries {
					if existing, ok := state[entry.IP]; !ok || entry.Gpc0 > existing {
						state[entry.IP] = entry.Gpc0
					}
				}
				mu.Unlock()
			}
		}(addr)
	}
	wg.Wait()
	return state, nil
}

// DumpBackends retrieves all currently blocked and unblocked IPs from HAProxy.

func (b *HAProxyBlocker) DumpBackends() ([]string, error) {

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
		allIPs = make(map[string]string) // Use map to store ip -> status, ensuring uniqueness

		mu sync.Mutex

		wg sync.WaitGroup

		errs = make(chan error, len(addresses))
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

			// 2. For each table, get all IPs (blocked and unblocked)

			for _, tableName := range tableNames {

				entriesInTable, err := b.getHAProxyAllIPsInTable(currentAddr, tableName)

				if err != nil {

					errs <- fmt.Errorf("failed to get IPs from table %s on %s: %w", tableName, currentAddr, err)

					return

				}

				mu.Lock()

				for _, entry := range entriesInTable {

					status := "U"

					if entry.Gpc0 > 0 {

						status = "B"

					}

					// If an IP is in multiple states, the last one seen wins. This is a reasonable

					// trade-off for this simple format.

					allIPs[entry.IP] = status

				}

				mu.Unlock()

			}

		}(addr)

	}

	wg.Wait()

	close(errs)

	if numErrs := len(errs); numErrs > 0 {

		b.P.Log(logging.LevelWarning, "HAPROXY_WARN", "One or more HAProxy instances failed to list IPs. Total failures: %d", numErrs)

		// Collect all errors

		var errorMessages []string

		for err := range errs {

			errorMessages = append(errorMessages, err.Error())

		}

		return nil, fmt.Errorf("%d HAProxy 'dump backends' commands failed: %s", numErrs, strings.Join(errorMessages, "; "))

	}

	var result []string

	for ip, status := range allIPs {

		result = append(result, fmt.Sprintf("%s|%s", ip, status))

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
		wg sync.WaitGroup

		mu sync.Mutex // Protects allBackendEntries and errors

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

				entries, err := b.getHAProxyAllIPsInTable(currentAddr, tableName)

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

					IP: ip,

					TableName: tableName,

					Reason: "Presence Mismatch",

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

							IP: ip,

							TableName: tableName,

							BackendA: presentBackends[0],

							BackendB: currentAddr,

							EntryA: firstEntry,

							EntryB: currentEntry,

							Reason: "Gpc0 Mismatch",

							Details: map[string]string{

								fmt.Sprintf("gpc0_%s", presentBackends[0]): fmt.Sprintf("%d", firstEntry.Gpc0),

								fmt.Sprintf("gpc0_%s", currentAddr): fmt.Sprintf("%d", currentEntry.Gpc0),
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

							IP: ip,

							TableName: tableName,

							BackendA: presentBackends[0],

							BackendB: currentAddr,

							EntryA: firstEntry,

							EntryB: currentEntry,

							Reason: "Expiration Mismatch",

							DiffMillis: diffExp,

							Details: map[string]string{

								fmt.Sprintf("exp_%s", presentBackends[0]): fmt.Sprintf("%d", firstEntry.Exp),

								fmt.Sprintf("exp_%s", currentAddr): fmt.Sprintf("%d", currentEntry.Exp),

								"diff_millis": fmt.Sprintf("%d", diffExp),

								"tolerance_millis": fmt.Sprintf("%d", expTolerance.Milliseconds()),
							},
						})

					}

				}

			}

		}

	}

	return discrepancies, nil

}

// Shutdown stops the health checker and cleans up resources.
func (b *HAProxyBlocker) Shutdown() {
	// Stop health checker if running
	select {
	case <-b.healthCheckStop:
		// Already stopped
	default:
		b.StopHealthCheck()
	}
}

// GetBackendHealth returns the health state for a backend address.
func (b *HAProxyBlocker) GetBackendHealth(addr string) (healthy bool, lastUptime int64, lastCheck time.Time) {
	b.healthMu.RLock()
	defer b.healthMu.RUnlock()

	if health, ok := b.backendHealth[addr]; ok {
		health.mu.RLock()
		defer health.mu.RUnlock()
		return health.Healthy, health.LastUptime, health.LastCheck
	}
	return true, 0, time.Time{}
}

// SetBackendHealth updates the health state for a backend address.
func (b *HAProxyBlocker) SetBackendHealth(addr string, healthy bool, uptime int64) {
	b.healthMu.RLock()
	health, ok := b.backendHealth[addr]
	b.healthMu.RUnlock()

	if !ok {
		b.healthMu.Lock()
		health = &BackendHealth{}
		b.backendHealth[addr] = health
		b.healthMu.Unlock()
	}

	health.mu.Lock()
	defer health.mu.Unlock()
	health.Healthy = healthy
	health.LastUptime = uptime
	health.LastCheck = time.Now()
}

// SetBackendNeedsResync marks a backend as needing resynchronization.
func (b *HAProxyBlocker) SetBackendNeedsResync(addr string, needsResync bool) {
	b.healthMu.RLock()
	health, ok := b.backendHealth[addr]
	b.healthMu.RUnlock()

	if !ok {
		return
	}

	health.mu.Lock()
	defer health.mu.Unlock()
	health.NeedsResync = needsResync
}

// StartHealthCheck starts the periodic health check goroutine.
func (b *HAProxyBlocker) StartHealthCheck(interval time.Duration) {
	if b.IsDryRun {
		return
	}

	b.healthCheckWg.Add(1)
	go b.healthCheckWorker(interval)
}

// StopHealthCheck stops the health check goroutine.
func (b *HAProxyBlocker) StopHealthCheck() {
	close(b.healthCheckStop)
	b.healthCheckWg.Wait()
}

// healthCheckWorker performs periodic health checks on all backends.
func (b *HAProxyBlocker) healthCheckWorker(interval time.Duration) {
	defer b.healthCheckWg.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-b.healthCheckStop:
			return
		case <-ticker.C:
			b.performHealthChecks()
		}
	}
}

// performHealthChecks checks all backends and updates their health state.
func (b *HAProxyBlocker) performHealthChecks() {
	addresses := b.P.GetBlockerAddresses()

	for _, addr := range addresses {
		wasHealthy, lastUptime, _ := b.GetBackendHealth(addr)

		uptime, err := b.GetHAProxyUptime(addr)
		if err != nil {
			// Backend is down or unreachable
			if wasHealthy {
				b.P.Log(logging.LevelWarning, "HEALTH_CHECK", "Backend %s became unhealthy: %v", addr, err)
			}
			b.SetBackendHealth(addr, false, 0)
			continue
		}

		// Backend is reachable
		needsResync := false
		if !wasHealthy {
			b.P.Log(logging.LevelInfo, "HEALTH_CHECK", "Backend %s recovered and is now healthy (resync needed)", addr)
			needsResync = true
		}

		// Check for uptime decrease (restart/reload)
		if wasHealthy && lastUptime > 0 && uptime < lastUptime {
			b.P.Log(logging.LevelWarning, "HEALTH_CHECK", "Backend %s restarted/reloaded (uptime: %d -> %d, resync needed)", addr, lastUptime, uptime)
			needsResync = true
		}

		b.SetBackendHealth(addr, true, uptime)

		if needsResync {
			b.SetBackendNeedsResync(addr, true)
		}
	}
}

// GetHAProxyUptime queries "show info" and returns the Uptime_sec value.
func (b *HAProxyBlocker) GetHAProxyUptime(addr string) (int64, error) {
	command := "show info\n"
	response, err := b.ExecuteHAProxyCommandFunc(addr, command)
	if err != nil {
		return 0, err
	}

	scanner := bufio.NewScanner(strings.NewReader(response))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Uptime_sec:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				uptimeStr := strings.TrimSpace(parts[1])
				uptime, parseErr := utils.ParseInt64(uptimeStr)
				if parseErr != nil {
					return 0, fmt.Errorf("failed to parse Uptime_sec value '%s': %w", uptimeStr, parseErr)
				}
				return uptime, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("uptime_sec not found in HAProxy info response")
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
		if strings.HasPrefix(line, "# table:") {
			// Line is like: '# table: <name>, type: ip, size:..., used:...'
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

// getHAProxyAllIPsInTable executes "show table <name>" and parses the response to get all IPs, regardless of gpc0 status.

func (b *HAProxyBlocker) getHAProxyAllIPsInTable(addr, tableName string) ([]HAProxyTableEntry, error) {
	command := fmt.Sprintf("show table %s\n", tableName)
	response, err := b.ExecuteHAProxyCommandFunc(addr, command)
	if err != nil {
		return nil, err
	}

	var entries []HAProxyTableEntry
	scanner := bufio.NewScanner(strings.NewReader(response))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "key=") {
			continue
		}

		var ip, expStr, gpc0Str string
		matches := haProxyTableEntryRegex.FindAllStringSubmatch(line, -1)
		for _, match := range matches {
			if len(match) > 1 {
				if ip == "" && match[haProxyTableEntryRegex.SubexpIndex("ip")] != "" {
					ip = match[haProxyTableEntryRegex.SubexpIndex("ip")]
				}
				if expStr == "" && match[haProxyTableEntryRegex.SubexpIndex("exp")] != "" {
					expStr = match[haProxyTableEntryRegex.SubexpIndex("exp")]
				}
				if gpc0Str == "" && match[haProxyTableEntryRegex.SubexpIndex("gpc0")] != "" {
					gpc0Str = match[haProxyTableEntryRegex.SubexpIndex("gpc0")]
				}
			}
		}

		if ip != "" && expStr != "" && gpc0Str != "" {
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
