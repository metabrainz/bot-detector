package main

import (
	"fmt"
	"net"
	"os"
	"regexp"
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
	for trackingKey, activity := range p.ActivityStore {
		if activity.IsBlocked && IsIPWhitelistedInList(trackingKey.IPInfo, currentWhitelist) {
			// IP is blocked AND now whitelisted -> unblock
			if err := p.UnblockIP(trackingKey.IPInfo); err != nil {
				// Log is handled inside UnblockIP
			} else {
				// Successful unblock
				activity.IsBlocked = false
				activity.BlockedUntil = time.Time{}
				p.LogFunc(LevelInfo, "WHITELIST_UNBLOCK", "Unblocked whitelisted IP %s (was blocked until %v).", trackingKey.IPInfo.Address, activity.BlockedUntil)
				unblockedCount++
			}
		}
	}

	if unblockedCount > 0 {
		p.LogFunc(LevelInfo, "WHITELIST_CLEANUP", "Finished Whitelist cleanup. Unblocked %d IPs.", unblockedCount)
	}
}

// LoadChainsFromYAML reads, parses, and pre-compiles regexes for the chains.
func LoadChainsFromYAML() ([]BehavioralChain, []*net.IPNet, []string, map[time.Duration]string, string, time.Duration, error) {
	data, err := os.ReadFile(YAMLFilePath)
	if err != nil {
		return nil, nil, nil, nil, "", 0, fmt.Errorf("failed to read YAML file %s: %w", YAMLFilePath, err)
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
		return nil, nil, nil, nil, "", 0, fmt.Errorf("failed to strictly unmarshal YAML (unknown field found): %w", err)
	}
	// ---------------------------------------------------------------------------------

	// Define the expected version for this application code.
	if config.Version == "" {
		// Enforce that the 'version' field must be present.
		return nil, nil, nil, nil, "", 0, fmt.Errorf("configuration file is missing the required 'version' field")
	}

	// 1. Check if the version is supported.
	isSupported := false
	for _, v := range SupportedConfigVersions {
		if config.Version == v {
			isSupported = true
			break
		}
	}

	if !isSupported {
		// 2. Report an error showing the unsupported version and the list of supported ones.
		supportedList := strings.Join(SupportedConfigVersions, ", ")
		return nil, nil, nil, nil, "", 0, fmt.Errorf(
			"configuration version mismatch: got '%s'. This version of the application supports: %s. Please update your YAML config file.",
			config.Version,
			supportedList,
		)
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
			return nil, nil, nil, nil, "", 0, fmt.Errorf("invalid CIDR in whitelist: %s: %w", cidr, err)
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
			return nil, nil, nil, nil, "", 0, fmt.Errorf("invalid duration '%s' in 'duration_tables': %w", durationStr, err)
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

	// Pre-parse the default block duration once.
	var defaultBlockDuration time.Duration
	if config.DefaultBlockDuration != "" {
		var err error
		defaultBlockDuration, err = time.ParseDuration(config.DefaultBlockDuration)
		if err != nil {
			return nil, nil, nil, nil, "", 0, fmt.Errorf("invalid block_duration format for default_block_duration: %w", err)
		}
	}

	for _, yamlChain := range config.Chains {
		var blockDuration time.Duration
		if yamlChain.BlockDuration != "" {
			var err error
			blockDuration, err = time.ParseDuration(yamlChain.BlockDuration)
			if err != nil {
				return nil, nil, nil, nil, "", 0, fmt.Errorf("chain '%s': invalid block_duration format: %w", yamlChain.Name, err)
			}
		} else {
			// If the chain's duration is not set, apply the pre-parsed default.
			blockDuration = defaultBlockDuration
		}

		// 4. Enforce that 'block' actions must have a non-zero duration.
		if yamlChain.Action == "block" && blockDuration == 0 {
			return nil, nil, nil, nil, "", 0, fmt.Errorf("chain '%s' has action 'block' but block_duration is missing or zero", yamlChain.Name)
		}

		// 2. Validate Match Key
		if yamlChain.MatchKey == "" {
			return nil, nil, nil, nil, "", 0, fmt.Errorf("chain '%s': match_key cannot be empty", yamlChain.Name)
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
				Order:           i + 1,
				FieldMatches:    yamlStep.FieldMatches,
				CompiledRegexes: make(map[string]*regexp.Regexp),
			}

			// Parse delays
			if yamlStep.MaxDelay != "" {
				runtimeStep.MaxDelayDuration, err = time.ParseDuration(yamlStep.MaxDelay)
				if err != nil {
					return nil, nil, nil, nil, "", 0, fmt.Errorf("chain '%s', step %d: invalid max_delay: %w", yamlChain.Name, runtimeStep.Order, err)
				}
			}
			if yamlStep.MinDelay != "" {
				runtimeStep.MinDelayDuration, err = time.ParseDuration(yamlStep.MinDelay)
				if err != nil {
					return nil, nil, nil, nil, "", 0, fmt.Errorf("chain '%s', step %d: invalid min_delay: %w", yamlChain.Name, runtimeStep.Order, err)
				}
			}
			if i == 0 && yamlStep.FirstHitSince != "" { // Only parse for the first step
				runtimeStep.FirstHitSinceDuration, err = time.ParseDuration(yamlStep.FirstHitSince)
				if err != nil {
					return nil, nil, nil, nil, "", 0, fmt.Errorf("chain '%s', step %d: invalid first_hit_since: %w", yamlChain.Name, runtimeStep.Order, err)
				}
			}

			// DIAGNOSTIC LOG: Check the loaded durations for all steps.
			// This is the key to debugging the silent load failure.
			LogOutput(LevelDebug, "CONFIG", "Chain '%s', Step %d: max_delay (raw: '%s', loaded: %v); min_delay (raw: '%s', loaded: %v)",
				yamlChain.Name, runtimeStep.Order,
				yamlStep.MaxDelay, runtimeStep.MaxDelayDuration,
				yamlStep.MinDelay, runtimeStep.MinDelayDuration)

			for field, regexStr := range yamlStep.FieldMatches {
				re, err := regexp.Compile(regexStr)
				if err != nil {
					return nil, nil, nil, nil, "", 0, fmt.Errorf("chain '%s', step %d, field '%s': failed to compile regex '%s': %w", yamlChain.Name, runtimeStep.Order, field, regexStr, err)
				}
				runtimeStep.CompiledRegexes[field] = re
			}
			runtimeChain.Steps = append(runtimeChain.Steps, runtimeStep)
		}
		newChains = append(newChains, runtimeChain)
	}

	// Find the maximum first_hit_since duration across all chains.
	var maxFirstHitSince time.Duration
	for _, chain := range newChains {
		if len(chain.Steps) > 0 && chain.Steps[0].FirstHitSinceDuration > maxFirstHitSince {
			maxFirstHitSince = chain.Steps[0].FirstHitSinceDuration
		}
	}

	return newChains, newWhitelistNets, config.HAProxyAddresses, newDurationTables, newFallbackName, maxFirstHitSince, nil
}

// ChainWatcher monitors the YAML config file for modifications and reloads the chains dynamically.
func (p *Processor) ChainWatcher() {
	if p.DryRun {
		return
	}

	// Enforce a minimum polling interval to prevent a tight loop on a zero-value duration.
	pollingInterval := p.Config.PollingInterval
	if pollingInterval < 1*time.Second {
		pollingInterval = 5 * time.Second // Default to a safe interval.
	}

	p.LogFunc(LevelDebug, "WATCH", "Starting ChainWatcher, polling %s every %v", YAMLFilePath, pollingInterval)
	for {
		time.Sleep(pollingInterval)

		fileInfo, err := os.Stat(YAMLFilePath)
		if err != nil {
			p.LogFunc(LevelError, "WATCH_ERROR", "Failed to stat file %s: %v", YAMLFilePath, err)
			continue
		}
		modTime := fileInfo.ModTime()

		// Read lock access for LastModTime check.
		p.ChainMutex.RLock()
		isChanged := modTime.After(p.Config.LastModTime)
		p.ChainMutex.RUnlock()

		if isChanged {
			p.LogFunc(LevelInfo, "WATCH", "Detected change in chains.yaml. Attempting reload...")

			// LoadChainsFromYAML now returns parsed data, not modifying global state.
			newChains, newWhitelistNets, newHAProxyAddrs, newDurationTables, newFallbackTable, maxFirstHitSince, err := LoadChainsFromYAML()
			if err != nil {
				p.LogFunc(LevelError, "LOAD_ERROR", "Failed to reload chains: %v", err)
				continue
			}

			// Update the processor's state with the new config.
			p.ChainMutex.Lock()
			p.Chains = newChains
			p.Config.WhitelistNets = newWhitelistNets
			p.Config.HAProxyAddresses = newHAProxyAddrs
			p.Config.DurationToTableName = newDurationTables
			p.Config.BlockTableNameFallback = newFallbackTable
			p.Config.MaxFirstHitSinceDuration = maxFirstHitSince
			p.Config.LastModTime = fileInfo.ModTime()
			p.ChainMutex.Unlock()

			// Cleanup any blocked IPs (This function still uses global state)
			p.CheckAndRemoveWhitelistedBlocks()
		}
	}
}
