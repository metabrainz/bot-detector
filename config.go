package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// CheckAndRemoveWhitelistedBlocks iterates over all IPs currently marked as blocked
// in the in-memory ActivityStore and unblocks them via HAProxy if they now fall
// within the newly loaded whitelist CIDRs.
func (p *Processor) CheckAndRemoveWhitelistedBlocks() {
	if p.DryRun {
		return
	}

	// 1. Acquire locks
	// The ActivityMutex must be held to safely iterate and modify the ActivityStore.
	p.ActivityMutex.Lock()
	defer p.ActivityMutex.Unlock()

	// Get the latest whitelist state (protected by ChainMutex)
	currentWhitelist := p.Config.WhitelistNets

	unblockedCount := 0

	// 2. Iterate blocked IPs
	for trackingKey, activityPtr := range p.ActivityStore {
		// Use the pointer to modify the original value in the map.
		// The 'activity' variable from the range loop is a copy.
		activity := activityPtr
		if activity.IsBlocked && IsIPWhitelistedInList(trackingKey.IPInfo, currentWhitelist) {
			// IP is blocked AND now whitelisted -> unblock
			if err := p.Blocker.Unblock(trackingKey.IPInfo); err != nil {
				// Log is handled inside UnblockIP
			} else {
				// Successful unblock
				p.LogFunc(LevelInfo, "WHITELIST_UNBLOCK", "Unblocked whitelisted IP %s (was blocked until %s).", trackingKey.IPInfo.Address, activity.BlockedUntil.Format(AppLogTimestampFormat))
				activity.IsBlocked = false
				activity.BlockedUntil = time.Time{}
				unblockedCount++
			}
		}
	}

	if unblockedCount > 0 {
		p.LogFunc(LevelInfo, "WHITELIST_CLEANUP", "Finished Whitelist cleanup. Unblocked %d IPs.", unblockedCount)
	}
}

// --- New Matcher Compilation Logic ---

// fieldMatcher is a function type that represents a compiled matching rule.
// It takes a LogEntry and returns true if the entry satisfies the rule.
type fieldMatcher func(entry *LogEntry) bool

// compileMatchers parses the raw `field_matches` interface from YAML into a slice of efficient matcher functions.
func compileMatchers(chainName string, stepIndex int, fieldMatches map[string]interface{}, fileDeps *[]string) ([]fieldMatcher, error) {
	var matchers []fieldMatcher
	for field, value := range fieldMatches {
		matcher, err := compileSingleMatcher(chainName, stepIndex, field, value, fileDeps)
		if err != nil {
			return nil, err // Propagate error up
		}
		matchers = append(matchers, matcher)
	}
	return matchers, nil
}

// compileSingleMatcher is a large switch that handles the different value "shapes" (string, int, list, map).
func compileSingleMatcher(chainName string, stepIndex int, field string, value interface{}, fileDeps *[]string) (fieldMatcher, error) {
	switch v := value.(type) {
	case string:
		return compileStringMatcher(chainName, stepIndex, field, v, fileDeps)
	case int:
		return compileIntMatcher(field, v), nil
	case []interface{}:
		return compileListMatcher(chainName, stepIndex, field, v, fileDeps)
	case map[string]interface{}:
		if field != "StatusCode" {
			return nil, fmt.Errorf("chain '%s', step %d: object matchers (gte, lt, etc.) are only supported for the 'StatusCode' field, not '%s'", chainName, stepIndex+1, field)
		}
		return compileObjectMatcher(chainName, stepIndex, field, v)
	default:
		return nil, fmt.Errorf("chain '%s', step %d, field '%s': unsupported value type '%T'", chainName, stepIndex+1, field, v)
	}
}

// readLinesFromFile is a helper to read a file into a slice of strings, ignoring comments and empty lines.
func readLinesFromFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Ignore empty lines and lines that start with '#'
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

// compileStringMatcher handles string values, which can be exact, regex, glob, or status code patterns.
func compileStringMatcher(chainName string, stepIndex int, field, value string, fileDeps *[]string) (fieldMatcher, error) {
	if strings.HasPrefix(value, "file:") {
		filePath := strings.TrimPrefix(value, "file:")
		*fileDeps = append(*fileDeps, filePath) // Track file for watching
		lines, err := readLinesFromFile(filePath)
		if err != nil {
			// Log a warning but do not fail the entire config load.
			// Treat the file as empty, effectively disabling this part of the rule.
			LogOutput(LevelWarning, "CONFIG_WARN", "Chain '%s', step %d, field '%s': failed to read file matcher '%s', it will be treated as empty: %v", chainName, stepIndex+1, field, filePath, err)
			// Return a matcher for an empty list, which will never match. Do not return an error.
			lines = []string{}
		}
		// Convert []string to []interface{} to reuse compileListMatcher
		interfaceSlice := make([]interface{}, len(lines))
		for i, v := range lines {
			interfaceSlice[i] = v
		}
		return compileListMatcher(chainName, stepIndex, field, interfaceSlice, fileDeps)
	}

	if strings.HasPrefix(value, "regex:") {
		pattern := strings.TrimPrefix(value, "regex:")
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("chain '%s', step %d, field '%s': invalid regex '%s': %w", chainName, stepIndex+1, field, pattern, err)
		}
		return func(entry *LogEntry) bool {
			fieldVal, _ := GetMatchValue(field, entry)
			return re.MatchString(fieldVal)
		}, nil
	}

	// Special handling for status code patterns like "4XX"
	if field == "StatusCode" && strings.HasSuffix(strings.ToUpper(value), "XX") {
		prefix := value[0:1] // "4" from "4XX"
		return func(entry *LogEntry) bool {
			return strings.HasPrefix(strconv.Itoa(entry.StatusCode), prefix)
		}, nil
	}

	// Default for string is exact match
	return func(entry *LogEntry) bool {
		fieldVal, _ := GetMatchValue(field, entry)
		return fieldVal == value
	}, nil
}

// compileIntMatcher handles exact integer matches.
func compileIntMatcher(field string, value int) fieldMatcher {
	return func(entry *LogEntry) bool {
		// This is optimized for StatusCode, the only integer field.
		if field == "StatusCode" {
			return entry.StatusCode == value
		}
		// Fallback for other potential future integer fields
		fieldValStr, _ := GetMatchValue(field, entry)
		fieldValInt, _ := strconv.Atoi(fieldValStr)
		return fieldValInt == value
	}
}

// compileListMatcher handles lists, creating an OR condition over its items.
func compileListMatcher(chainName string, stepIndex int, field string, values []interface{}, fileDeps *[]string) (fieldMatcher, error) {
	var subMatchers []fieldMatcher
	for _, item := range values {
		matcher, err := compileSingleMatcher(chainName, stepIndex, field, item, fileDeps)
		if err != nil {
			return nil, err // Error in a sub-matcher
			return nil, err
		}
		subMatchers = append(subMatchers, matcher)
	}

	return func(entry *LogEntry) bool {
		for _, matcher := range subMatchers {
			if matcher(entry) {
				return true // OR logic: one match is enough
			}
		}
		return false
	}, nil
}

// compileObjectMatcher handles map values, creating an AND condition for numeric ranges.
func compileObjectMatcher(chainName string, stepIndex int, field string, obj map[string]interface{}) (fieldMatcher, error) {
	var subMatchers []fieldMatcher

	for key, val := range obj {
		num, ok := val.(int)
		if !ok {
			return nil, fmt.Errorf("chain '%s', step %d, field '%s': value for '%s' must be an integer, got %T", chainName, stepIndex+1, field, key, val)
		}

		var matcher fieldMatcher
		switch key {
		case "gt":
			matcher = func(entry *LogEntry) bool { return entry.StatusCode > num }
		case "gte":
			matcher = func(entry *LogEntry) bool { return entry.StatusCode >= num }
		case "lt":
			matcher = func(entry *LogEntry) bool { return entry.StatusCode < num }
		case "lte":
			matcher = func(entry *LogEntry) bool { return entry.StatusCode <= num }
		default:
			return nil, fmt.Errorf("chain '%s', step %d, field '%s': unknown operator '%s' in object matcher", chainName, stepIndex+1, field, key)
		}
		subMatchers = append(subMatchers, matcher)
	}

	if len(subMatchers) == 0 {
		return nil, errors.New("object matcher must not be empty")
	}

	return func(entry *LogEntry) bool {
		for _, matcher := range subMatchers {
			if !matcher(entry) {
				return false // AND logic: one failure means total failure
			}
		}
		return true
	}, nil
}

// LoadChainsFromYAML reads, parses, and pre-compiles regexes for the chains.
func LoadChainsFromYAML() (*LoadedConfig, error) {
	data, err := os.ReadFile(YAMLFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read YAML file %s: %w", YAMLFilePath, err)
	}

	var config ChainConfig

	// 1. Create a new decoder from the YAML data string.
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))

	// 2. Set the public KnownFields field to true to enforce strict decoding.
	// This makes Decode fail if an unknown key is encountered in the YAML.
	decoder.KnownFields(true)

	// 3. Decode the YAML using the strict decoder.
	if err := decoder.Decode(&config); err != nil {
		// This error will now explicitly include the unknown field error message.
		return nil, fmt.Errorf("failed to strictly unmarshal YAML (unknown field found): %w", err)
	}
	// ---------------------------------------------------------------------------------

	// Define the supported versions for this application code.
	if config.Version == "" {
		// Enforce that the 'version' field must be present.
		return nil, fmt.Errorf("configuration file is missing the required 'version' field")
	}

	// Check if the version is supported.
	isSupported := false
	for _, v := range SupportedConfigVersions {
		if config.Version == v {
			isSupported = true
			break // Found a supported version
		}
	}

	if !isSupported {
		// Report an error showing the unsupported version and the list of supported ones.
		supportedList := strings.Join(SupportedConfigVersions, ", ")
		return nil, fmt.Errorf(
			"configuration version mismatch: got '%s'. This application supports: %s. Please update your YAML config file.",
			config.Version,
			supportedList,
		)
	}

	// --- PARSE GLOBAL SETTINGS ---
	var pollingInterval, cleanupInterval, idleTimeout, outOfOrderTolerance time.Duration

	// Set defaults for global settings
	logLevelStr := DefaultLogLevel
	pollingIntervalStr := DefaultPollingInterval
	cleanupIntervalStr := DefaultCleanupInterval
	idleTimeoutStr := DefaultIdleTimeout
	outOfOrderToleranceStr := DefaultOutOfOrderTolerance

	// Override defaults with values from YAML if they exist
	if config.LogLevel != "" {
		logLevelStr = config.LogLevel
	}
	if config.PollingInterval != "" {
		pollingIntervalStr = config.PollingInterval
	}
	if config.CleanupInterval != "" {
		cleanupIntervalStr = config.CleanupInterval
	}
	if config.IdleTimeout != "" {
		idleTimeoutStr = config.IdleTimeout
	}
	if config.OutOfOrderTolerance != "" {
		outOfOrderToleranceStr = config.OutOfOrderTolerance
	}

	// Parse durations
	pollingInterval, err = time.ParseDuration(pollingIntervalStr)
	if err != nil {
		return nil, fmt.Errorf("invalid poll_interval format: %w", err)
	}
	cleanupInterval, err = time.ParseDuration(cleanupIntervalStr)
	if err != nil {
		return nil, fmt.Errorf("invalid cleanup_interval format: %w", err)
	}
	idleTimeout, err = time.ParseDuration(idleTimeoutStr)
	if err != nil {
		return nil, fmt.Errorf("invalid idle_timeout format: %w", err)
	}
	outOfOrderTolerance, err = time.ParseDuration(outOfOrderToleranceStr)
	if err != nil {
		return nil, fmt.Errorf("invalid out_of_order_tolerance format: %w", err)
	}

	// Parse Whitelist CIDRs
	newWhitelistNets := make([]*net.IPNet, 0)
	for _, cidr := range config.WhitelistCIDRs {
		normalizedCIDR := cidr

		// Check if the input is a bare IP (no '/') and is a valid IP address.
		// If it's a bare IP, append the appropriate mask (/32 for IPv4, /128 for IPv6)
		if !strings.Contains(cidr, "/") {
			if ip := net.ParseIP(cidr); ip != nil {
				if ip.To4() != nil {
					// It's a bare IPv4 address
					normalizedCIDR = cidr + "/32"
				} else {
					// It's a bare IPv6 address
					normalizedCIDR = cidr + "/128"
				}
			} else {
				// Not a valid bare IP, will fail net.ParseCIDR below
				normalizedCIDR = cidr
			}
		}

		// net.ParseCIDR returns the IP and the IPNet. The IPNet is what we store for comparison.
		_, ipNet, err := net.ParseCIDR(normalizedCIDR)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR in whitelist: %s: %w", cidr, err)
		}
		newWhitelistNets = append(newWhitelistNets, ipNet)
	}

	// --- PARSE DURATION TABLES ---
	newDurationTables := make(map[time.Duration]string, len(config.DurationTables))
	longestDuration := 0 * time.Second
	newFallbackName := ""

	for durationStr, tableName := range config.DurationTables {
		duration, err := time.ParseDuration(durationStr)
		if err != nil {
			return nil, fmt.Errorf("invalid duration '%s' in 'duration_tables': %w", durationStr, err)
		}
		newDurationTables[duration] = tableName

		// Find the longest duration to set the fallback table name
		if duration > longestDuration {
			longestDuration = duration
			newFallbackName = tableName
		}
	}

	// --- PARSE HAPROXY TIMEOUTS ---
	var haProxyMaxRetries int
	var haProxyRetryDelay, haProxyDialTimeout time.Duration

	if config.HAProxyMaxRetries > 0 {
		haProxyMaxRetries = config.HAProxyMaxRetries
	} else {
		haProxyMaxRetries = DefaultHAProxyMaxRetries
	}

	if config.HAProxyRetryDelay != "" {
		haProxyRetryDelay, err = time.ParseDuration(config.HAProxyRetryDelay)
		if err != nil {
			return nil, fmt.Errorf("invalid haproxy_retry_delay: %w", err)
		}
	} else {
		haProxyRetryDelay = DefaultHAProxyRetryDelay
	}

	if config.HAProxyDialTimeout != "" {
		haProxyDialTimeout, err = time.ParseDuration(config.HAProxyDialTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid haproxy_dial_timeout: %w", err)
		}
	} else {
		haProxyDialTimeout = DefaultHAProxyDialTimeout
	}

	// --- PARSE CHAINS ---
	newChains := make([]BehavioralChain, 0)

	// Pre-parse the default block duration once.
	var defaultBlockDuration time.Duration
	if config.DefaultBlockDuration != "" {
		var err error
		defaultBlockDuration, err = time.ParseDuration(config.DefaultBlockDuration)
		if err != nil {
			return nil, fmt.Errorf("invalid block_duration format for default_block_duration: %w", err)
		}
	}

	// --- PARSE FILE DEPENDENCIES ---
	fileDependencies := []string{}

	for _, yamlChain := range config.Chains {
		var blockDuration time.Duration
		if yamlChain.BlockDuration != "" {
			var err error
			blockDuration, err = time.ParseDuration(yamlChain.BlockDuration)
			if err != nil {
				return nil, fmt.Errorf("chain '%s': invalid block_duration format: %w", yamlChain.Name, err)
			}
		} else {
			// If the chain's duration is not set, apply the pre-parsed default.
			blockDuration = defaultBlockDuration
		}

		// 4. Enforce that 'block' actions must have a non-zero duration.
		if yamlChain.Action == "block" && blockDuration == 0 {
			return nil, fmt.Errorf("chain '%s' has action 'block' but block_duration is missing or zero", yamlChain.Name)
		}

		// 2. Validate Match Key
		if yamlChain.MatchKey == "" {
			return nil, fmt.Errorf("chain '%s': match_key cannot be empty", yamlChain.Name)
		}

		runtimeChain := BehavioralChain{
			Name:          yamlChain.Name,
			Action:        yamlChain.Action,
			BlockDuration: blockDuration,
			MatchKey:      yamlChain.MatchKey,
		}

		// 3. Process Steps
		for i, yamlStep := range yamlChain.Steps {
			runtimeStep := StepDef{
				Order: i + 1,
				// FieldMatches is no longer stored directly in the runtime step.
				// It's compiled into Matchers.
			}

			// Parse delays
			if yamlStep.MaxDelay != "" {
				runtimeStep.MaxDelayDuration, err = time.ParseDuration(yamlStep.MaxDelay)
				if err != nil {
					return nil, fmt.Errorf("chain '%s', step %d: invalid max_delay: %w", yamlChain.Name, runtimeStep.Order, err)
				}
			}
			if yamlStep.MinDelay != "" {
				runtimeStep.MinDelayDuration, err = time.ParseDuration(yamlStep.MinDelay)
				if err != nil {
					return nil, fmt.Errorf("chain '%s', step %d: invalid min_delay: %w", yamlChain.Name, runtimeStep.Order, err)
				}
			}
			if i == 0 && yamlStep.MinTimeSinceLastHit != "" { // Only parse for the first step
				runtimeStep.MinTimeSinceLastHit, err = time.ParseDuration(yamlStep.MinTimeSinceLastHit)
				if err != nil {
					return nil, fmt.Errorf("chain '%s', step %d: invalid min_time_since_last_hit: %w", yamlChain.Name, runtimeStep.Order, err)
				}
			}

			// DIAGNOSTIC LOG: Check the loaded durations for all steps.
			// This is useful for debugging duration parsing.
			LogOutput(LevelDebug, "CONFIG", "Chain '%s', Step %d: max_delay (raw: '%s', loaded: %v); min_delay (raw: '%s', loaded: %v)",
				yamlChain.Name, runtimeStep.Order,
				yamlStep.MaxDelay, runtimeStep.MaxDelayDuration,
				yamlStep.MinDelay, runtimeStep.MinDelayDuration)

			// Compile the new flexible matchers
			runtimeStep.Matchers, err = compileMatchers(yamlChain.Name, i, yamlStep.FieldMatches, &fileDependencies)
			if err != nil {
				return nil, err // Error from compilation
			}
			runtimeChain.Steps = append(runtimeChain.Steps, runtimeStep)
		}
		newChains = append(newChains, runtimeChain)
	}

	// After parsing all chains, check if any use the 'block' action.
	// If so, and no duration tables are configured, issue a single warning.
	if longestDuration == 0*time.Second {
		for _, chain := range newChains {
			if chain.Action == "block" {
				// Downgrade this warning to Debug level during test runs to reduce noise.
				logLevel := LevelWarning
				if isTesting() {
					logLevel = LevelDebug
				}
				LogOutput(logLevel, "CONFIG", "One or more chains use the 'block' action, but no 'duration_tables' are configured. All block attempts will be skipped.")
				break // We only need to log this warning once.
			}
		}
	}

	// Find the maximum min_time_since_last_hit duration across all chains for cleanup optimization.
	var maxTimeSinceLastHit time.Duration
	for _, chain := range newChains {
		if len(chain.Steps) > 0 && chain.Steps[0].MinTimeSinceLastHit > maxTimeSinceLastHit {
			maxTimeSinceLastHit = chain.Steps[0].MinTimeSinceLastHit
		}
	}

	return &LoadedConfig{
		Chains:                 newChains,
		WhitelistNets:          newWhitelistNets,
		HAProxyAddresses:       config.HAProxyAddresses,
		HAProxyMaxRetries:      haProxyMaxRetries,
		HAProxyRetryDelay:      haProxyRetryDelay,
		HAProxyDialTimeout:     haProxyDialTimeout,
		DurationToTableName:    newDurationTables,
		BlockTableNameFallback: newFallbackName,
		PollingInterval:        pollingInterval,
		CleanupInterval:        cleanupInterval,
		IdleTimeout:            idleTimeout,
		OutOfOrderTolerance:    outOfOrderTolerance,
		LogLevel:               logLevelStr,
		MaxTimeSinceLastHit:    maxTimeSinceLastHit,
		FileDependencies:       fileDependencies,
	}, nil
}

// ChainWatcher monitors the YAML config file for modifications and reloads the chains dynamically.
func (p *Processor) ChainWatcher(stop <-chan struct{}) {
	if p.DryRun {
		return
	}

	// Enforce a minimum polling interval to prevent a tight loop on a zero-value duration.
	var pollingInterval time.Duration
	if p.Config.testOverridePollingInterval > 0 {
		pollingInterval = p.Config.testOverridePollingInterval // Use test override if set
	} else if p.Config.PollingInterval < 1*time.Second {
		// In production, enforce a minimum safe interval.
		pollingInterval = 5 * time.Second // Default to a safe interval.
	}

	p.LogFunc(LevelDebug, "WATCH", "Starting ChainWatcher, polling every %v", pollingInterval)
	for {
		select {
		case <-stop:
			p.LogFunc(LevelInfo, "WATCH", "ChainWatcher received stop signal. Shutting down.")
			return
		case <-time.After(pollingInterval):
			// Continue with polling
		}

		isChanged := false
		changedFile := ""

		// 1. Check the main YAML file
		fileInfo, err := os.Stat(YAMLFilePath)
		if err != nil {
			p.LogFunc(LevelError, "WATCH_ERROR", "Failed to stat file %s: %v", YAMLFilePath, err)
			continue
		}

		p.ChainMutex.RLock()
		if fileInfo.ModTime().After(p.Config.LastModTime) {
			isChanged = true
			changedFile = YAMLFilePath
		} else {
			// 2. Check all file dependencies if YAML hasn't changed
			for _, depPath := range p.Config.FileDependencies {
				depInfo, err := os.Stat(depPath)
				if err != nil {
					// Log at debug level if a dependency file is missing, as this might be temporary.
					// A reload will only be triggered if chains.yaml itself changes.
					p.LogFunc(LevelDebug, "WATCH_SKIP", "Could not stat dependency file %s (may have been removed): %v", depPath, err)
					continue
				}
				if depInfo.ModTime().After(p.Config.LastModTime) {
					isChanged = true
					changedFile = depPath
					break
				}
			}
		}
		p.ChainMutex.RUnlock()

		if isChanged {
			p.LogFunc(LevelInfo, "WATCH", "Detected change in '%s'. Attempting reload...", changedFile)

			// LoadChainsFromYAML now returns parsed data, not modifying global state.
			loadedCfg, err := LoadChainsFromYAML()
			if err != nil {
				p.LogFunc(LevelError, "LOAD_ERROR", "Failed to reload chains: %v", err)
				continue
			}

			// Update the processor's state with the new config.
			p.ChainMutex.Lock()
			p.Chains = loadedCfg.Chains
			p.Config.WhitelistNets = loadedCfg.WhitelistNets
			p.Config.HAProxyAddresses = loadedCfg.HAProxyAddresses
			p.Config.HAProxyMaxRetries = loadedCfg.HAProxyMaxRetries
			p.Config.HAProxyRetryDelay = loadedCfg.HAProxyRetryDelay
			p.Config.HAProxyDialTimeout = loadedCfg.HAProxyDialTimeout
			p.Config.DurationToTableName = loadedCfg.DurationToTableName
			p.Config.BlockTableNameFallback = loadedCfg.BlockTableNameFallback
			p.Config.PollingInterval = loadedCfg.PollingInterval
			p.Config.CleanupInterval = loadedCfg.CleanupInterval
			p.Config.IdleTimeout = loadedCfg.IdleTimeout
			p.Config.OutOfOrderTolerance = loadedCfg.OutOfOrderTolerance
			SetLogLevel(loadedCfg.LogLevel) // Update log level dynamically
			p.Config.MaxTimeSinceLastHit = loadedCfg.MaxTimeSinceLastHit
			p.Config.FileDependencies = loadedCfg.FileDependencies
			p.Config.LastModTime = time.Now() // Use time.Now() to avoid race conditions with fast edits
			p.ChainMutex.Unlock()

			// Cleanup any blocked IPs (This function still uses global state)
			p.CheckAndRemoveWhitelistedBlocks()
		}
	}
}
