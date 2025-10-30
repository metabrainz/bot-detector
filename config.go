package main

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// --- GLOBAL STATE ---
var (
	Chains      []BehavioralChain
	ChainMutex  sync.RWMutex
	LastModTime time.Time

	WhitelistNets []*net.IPNet // Holds parsed CIDR networks

	HAProxyAddresses []string     // Parsed list of HAProxy "host:port" addresses
	HAProxyMutex     sync.RWMutex // Mutex for HAProxyAddresses

	DurationToTableName map[time.Duration]string // Map parsed durations to table names
	DurationTableMutex  sync.RWMutex             // Mutex for DurationToTableName

	// Define the fallback table name for the longest duration
	BlockTableNameFallback string
)

// CheckAndRemoveWhitelistedBlocks iterates over all IPs currently marked as blocked
// in the in-memory ActivityStore and unblocks them via HAProxy if they now fall
// within the newly loaded whitelist CIDRs.
func CheckAndRemoveWhitelistedBlocks() {
	if DryRun {
		return
	}

	// 1. Acquire locks
	// The ActivityMutex must be held to safely iterate and modify the ActivityStore.
	ActivityMutex.Lock()
	defer ActivityMutex.Unlock()

	// Get the latest whitelist state (protected by ChainMutex)
	ChainMutex.RLock()
	currentWhitelist := WhitelistNets
	ChainMutex.RUnlock()

	unblockedCount := 0

	// 2. Iterate blocked IPs
	for trackingKey, activity := range ActivityStore {
		// Blocking is always IP-based, so skip IP+UA keys
		if trackingKey.UA != "" {
			continue
		}

		if activity.IsBlocked {
			ip := net.ParseIP(trackingKey.IP)
			if ip == nil {
				continue
			}

			// 3. Check if the blocked IP is now whitelisted.
			isNowWhitelisted := false
			for _, ipNet := range currentWhitelist {
				if ipNet.Contains(ip) {
					isNowWhitelisted = true
					break
				}
			}

			if isNowWhitelisted {
				// 4. If whitelisted, unblock in HAProxy (error logged internally in UnblockIP).
				UnblockIP(trackingKey.IP)

				// 5. Reset the in-memory block state for correctness.
				activity.IsBlocked = false
				activity.BlockedUntil = time.Time{}
				unblockedCount++
			}
		}
	}

	if unblockedCount > 0 {
		LogOutput(LevelInfo, "CONFIG", "Unblocked %d IPs from HAProxy block tables that are now included in the new whitelist.", unblockedCount)
	}
}

// LoadChainsFromYAML reads, parses, and pre-compiles regexes for the chains.
func LoadChainsFromYAML() ([]BehavioralChain, error) {
	data, err := os.ReadFile(YAMLFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read YAML file %s: %w", YAMLFilePath, err)
	}

	var config ChainConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal YAML: %w", err)
	}

	// Parse Whitelist CIDRs
	newWhitelistNets := make([]*net.IPNet, 0)
	for _, cidr := range config.WhitelistCIDRs {
		// net.ParseCIDR returns the IP and the IPNet. The IPNet is what we store for comparison.
		_, ipNet, err := net.ParseCIDR(cidr)
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

	if longestDuration == 0*time.Second {
		// Log a warning if no tables are configured, but allow startup for testing.
		LogOutput(LevelWarning, "CONFIG", "No HAProxy duration tables configured. All block attempts will be skipped.")
	}

	// --- PARSE CHAINS ---
	newChains := make([]BehavioralChain, 0)

	for _, yamlChain := range config.Chains {
		// Determine the final duration string to use.
		blockDurationStr := yamlChain.BlockDuration

		// 1. Check if the chain has an empty block_duration string
		if blockDurationStr == "" {
			// 2. Apply the top-level default if it is set
			if config.DefaultBlockDuration != "" {
				blockDurationStr = config.DefaultBlockDuration
			}
		}

		// 3. Parse Block Duration (using the potentially defaulted value)
		var blockDuration time.Duration
		var err error

		// Only attempt to parse if we have a non-empty string.
		// If blockDurationStr is still empty, blockDuration remains 0, which is acceptable for 'log' actions.
		if blockDurationStr != "" {
			blockDuration, err = time.ParseDuration(blockDurationStr)
			if err != nil {
				return nil, fmt.Errorf("chain '%s': invalid block_duration format: %w", yamlChain.Name, err)
			}
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
		for _, yamlStep := range yamlChain.Steps {
			runtimeStep := StepDef{
				Order:           yamlStep.Order,
				FieldMatches:    yamlStep.FieldMatches,
				CompiledRegexes: make(map[string]*regexp.Regexp),
			}

			// Parse delays
			if yamlStep.MaxDelaySeconds != "" {
				runtimeStep.MaxDelayDuration, err = time.ParseDuration(yamlStep.MaxDelaySeconds)
				if err != nil {
					return nil, fmt.Errorf("chain '%s', step %d: invalid max_delay: %w", yamlChain.Name, yamlStep.Order, err)
				}
			}
			if yamlStep.MinDelaySeconds != "" {
				runtimeStep.MinDelayDuration, err = time.ParseDuration(yamlStep.MinDelaySeconds)
				if err != nil {
					return nil, fmt.Errorf("chain '%s', step %d: invalid min_delay: %w", yamlChain.Name, yamlStep.Order, err)
				}
			}

			for field, regexStr := range yamlStep.FieldMatches {
				re, err := regexp.Compile(regexStr)
				if err != nil {
					return nil, fmt.Errorf("chain '%s', step %d, field '%s': failed to compile regex '%s': %w", yamlChain.Name, yamlStep.Order, field, regexStr, err)
				}
				runtimeStep.CompiledRegexes[field] = re
			}
			runtimeChain.Steps = append(runtimeChain.Steps, runtimeStep)
		}
		newChains = append(newChains, runtimeChain)
	}

	// --- GLOBAL STATE UPDATE ---
	// 1. Chains, LastModTime, and Whitelist
	ChainMutex.Lock()
	Chains = newChains
	LastModTime = time.Now()
	WhitelistNets = newWhitelistNets
	ChainMutex.Unlock()

	// 2. Update HAProxy addresses
	HAProxyMutex.Lock()
	HAProxyAddresses = config.HAProxyAddresses // Assuming config.HAProxyAddresses is the list
	HAProxyMutex.Unlock()

	// 3. Update Duration Tables
	DurationTableMutex.Lock()
	DurationToTableName = newDurationTables
	BlockTableNameFallback = newFallbackName
	DurationTableMutex.Unlock()

	LogOutput(LevelInfo, "CONFIG", "Loaded %d chains, %d whitelist CIDRs, %d HAProxy addresses, and %d duration tables.", len(Chains), len(WhitelistNets), len(HAProxyAddresses), len(newDurationTables))

	return newChains, nil
}

// ChainWatcher monitors the YAML config file for modifications and reloads the chains dynamically.
func ChainWatcher() {
	if DryRun {
		return
	}
	LogOutput(LevelDebug, "WATCH", "Starting ChainWatcher, polling %s every %v", YAMLFilePath, PollingInterval)
	for {
		time.Sleep(PollingInterval)

		fileInfo, err := os.Stat(YAMLFilePath)
		if err != nil {
			LogOutput(LevelError, "WATCH_ERROR", "Failed to stat file %s: %v", YAMLFilePath, err)
			continue
		}
		modTime := fileInfo.ModTime()

		// Read lock access for LastModTime check.
		ChainMutex.RLock()
		isChanged := modTime.After(LastModTime)
		ChainMutex.RUnlock()

		if isChanged {
			LogOutput(LevelInfo, "WATCH", "Detected change in chains.yaml. Attempting reload...")

			// LoadChainsFromYAML updates the global state (Chains, WhitelistNets, etc.)
			_, err := LoadChainsFromYAML()
			if err != nil {
				LogOutput(LevelError, "LOAD_ERROR", "Failed to reload chains: %v", err)
				continue
			}

			// NEW: Cleanup any blocked IPs that are now whitelisted.
			// This must be run AFTER LoadChainsFromYAML has successfully updated WhitelistNets.
			CheckAndRemoveWhitelistedBlocks()

			// ... (rest of ChainWatcher logic, if any)
		}
	}
}
