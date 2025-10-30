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
		ip, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR in whitelist: %s: %w", cidr, err)
		}
		newWhitelistNets = append(newWhitelistNets, ipNet)
		LogOutput(LevelInfo, "WHITELIST", "Added CIDR to Whitelist: %s (Base IP: %s)", cidr, ip)
	}
	ChainMutex.Lock()
	WhitelistNets = newWhitelistNets // Update the global list atomically
	ChainMutex.Unlock()

	newChains := make([]BehavioralChain, 0, len(config.Chains))

	for _, yamlChain := range config.Chains {
		runtimeChain := BehavioralChain{
			Name:     yamlChain.Name,
			Action:   yamlChain.Action,
			MatchKey: yamlChain.MatchKey,
		}

		if runtimeChain.MatchKey == "" {
			runtimeChain.MatchKey = "ip"
		}
		validKeys := map[string]bool{"ip": true, "ipv4": true, "ipv6": true, "ip_ua": true, "ipv4_ua": true, "ipv6_ua": true}
		keyLower := strings.ToLower(runtimeChain.MatchKey)

		if !validKeys[keyLower] {
			return nil, fmt.Errorf("chain '%s': invalid match_key '%s'. MatchKey must be one of: ip, ipv4, ipv6, ip_ua, ipv4_ua, ipv6_ua", yamlChain.Name, runtimeChain.MatchKey)
		}
		runtimeChain.MatchKey = keyLower

		if runtimeChain.Action != "block" && runtimeChain.Action != "log" {
			return nil, fmt.Errorf("chain '%s': invalid action '%s'. Action must be 'block' or 'log'", yamlChain.Name, runtimeChain.Action)
		}
		if yamlChain.BlockDuration != "" {
			runtimeChain.BlockDuration, err = time.ParseDuration(yamlChain.BlockDuration)
			if err != nil {
				return nil, fmt.Errorf("chain '%s': failed to parse block_duration '%s': %w", yamlChain.Name, yamlChain.BlockDuration, err)
			}
		}

		for _, yamlStep := range yamlChain.Steps {
			runtimeStep := StepDef{
				Order:           yamlStep.Order,
				FieldMatches:    yamlStep.FieldMatches,
				CompiledRegexes: make(map[string]*regexp.Regexp),
			}

			if yamlStep.MaxDelaySeconds != "" {
				runtimeStep.MaxDelayDuration, err = time.ParseDuration(yamlStep.MaxDelaySeconds)
				if err != nil {
					return nil, fmt.Errorf("chain '%s', step %d: failed to parse max_delay '%s': %w", yamlChain.Name, yamlStep.Order, yamlStep.MaxDelaySeconds, err)
				}
			}

			if yamlStep.MinDelaySeconds != "" {
				runtimeStep.MinDelayDuration, err = time.ParseDuration(yamlStep.MinDelaySeconds)
				if err != nil {
					return nil, fmt.Errorf("chain '%s', step %d: failed to parse min_delay '%s': %w", yamlChain.Name, yamlStep.Order, yamlStep.MinDelaySeconds, err)
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

		if modTime.After(LastModTime) {
			LogOutput(LevelInfo, "WATCH", "Detected change in chains.yaml. Attempting reload...")

			newChains, err := LoadChainsFromYAML()
			if err != nil {
				LogOutput(LevelError, "LOAD_ERROR", "Failed to reload chains: %v. Retaining previous configuration.", err)
				continue
			}

			ChainMutex.Lock()
			Chains = newChains
			LastModTime = modTime
			ChainMutex.Unlock()
			LogOutput(LevelInfo, "LOAD", "Successfully reloaded and compiled %d behavioral chains.", len(newChains))
		}
	}
}
