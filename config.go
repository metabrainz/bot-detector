package main

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
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

	// --- PARSE HAPROXY ADDRESSES ---
	// Clean the unmarshaled []string slice by trimming whitespace.
	cleanedAddresses := make([]string, 0, len(config.HAProxyAddresses))
	for _, addr := range config.HAProxyAddresses {
		trimmed := strings.TrimSpace(addr)
		if trimmed != "" {
			cleanedAddresses = append(cleanedAddresses, trimmed)
		}
	}

	// Log a warning if no addresses are configured, but allow startup.
	if len(cleanedAddresses) == 0 {
		LogOutput(LevelWarning, "CONFIG", "No HAProxy addresses configured in YAML 'haproxy_addresses' field. Blocking actions will be skipped.")
	}

	// --- PARSE BEHAVIORAL CHAINS ---
	newChains := make([]BehavioralChain, 0, len(config.Chains))
	for _, yamlChain := range config.Chains {
		runtimeChain := BehavioralChain{
			Name:     yamlChain.Name,
			Action:   yamlChain.Action,
			MatchKey: yamlChain.MatchKey,
		}

		// Block Duration parsing
		if runtimeChain.Action == "block" && yamlChain.BlockDuration != "" {
			d, err := time.ParseDuration(yamlChain.BlockDuration)
			if err != nil {
				return nil, fmt.Errorf("chain '%s': invalid block_duration format: %w", runtimeChain.Name, err)
			}
			runtimeChain.BlockDuration = d
		} else if runtimeChain.Action == "block" {
			return nil, fmt.Errorf("chain '%s': action 'block' requires a 'block_duration'", runtimeChain.Name)
		}

		// Step parsing
		for _, yamlStep := range yamlChain.Steps {
			runtimeStep := StepDef{
				Order:           yamlStep.Order,
				FieldMatches:    yamlStep.FieldMatches,
				CompiledRegexes: make(map[string]*regexp.Regexp),
			}

			// Delay parsing
			if yamlStep.MaxDelaySeconds != "" {
				d, err := time.ParseDuration(yamlStep.MaxDelaySeconds)
				if err != nil {
					return nil, fmt.Errorf("chain '%s', step %d: invalid max_delay format: %w", yamlChain.Name, yamlStep.Order, err)
				}
				runtimeStep.MaxDelayDuration = d
			}
			if yamlStep.MinDelaySeconds != "" {
				d, err := time.ParseDuration(yamlStep.MinDelaySeconds)
				if err != nil {
					return nil, fmt.Errorf("chain '%s', step %d: invalid min_delay format: %w", yamlChain.Name, yamlStep.Order, err)
				}
				runtimeStep.MinDelayDuration = d
			}

			// Regex compilation
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

	// --- ATOMIC UPDATE BLOCK (Ensures safe, dynamic configuration update) ---

	// 1. Update primary chains and whitelist
	ChainMutex.Lock()
	Chains = newChains
	LastModTime = time.Now()
	WhitelistNets = newWhitelistNets
	ChainMutex.Unlock()

	// 2. Update HAProxy addresses
	HAProxyMutex.Lock()
	HAProxyAddresses = cleanedAddresses
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

			_, err := LoadChainsFromYAML()
			if err != nil {
				LogOutput(LevelError, "LOAD_ERROR", "Failed to reload chains: %v. Continuing with old configuration.", err)
			}
		}
	}
}
