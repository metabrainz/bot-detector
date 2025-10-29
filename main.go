package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// --- CONSTANT FOR CRITICAL LOG LINE BUFFER LIMIT ---
// If a line exceeds this limit (e.g., 16KB), it is skipped entirely.
const MaxLogLineSize = 16 * 1024

// Custom error type for skipped lines
var ErrLineSkipped = errors.New("line exceeded critical limit and was skipped")

// --- CONFIGURATION GLOBAL VARS (Set by CLI flags) ---
var (
	LogFilePath       string
	HAProxySocketPath string
	BlockedMapPath    string

	YAMLFilePath       string
	PollingIntervalStr string

	CleanupIntervalStr string
	IdleTimeoutStr     string // Duration an IP must be inactive before its state is purged.

	LogLevelStr string
	DryRun      bool
	TestLogPath string

	PollingInterval time.Duration
	CleanupInterval time.Duration
	IdleTimeout     time.Duration
)

// --- LOGGING STRUCTURE ---
type LogLevel int

const (
	LevelCritical LogLevel = iota // 0: Highest priority: Blocks, Fatal errors
	LevelError                    // 1: Critical failure, but program continues
	LevelWarning                  // 2: Non-critical issues, time parse warnings (Default Level)
	LevelInfo                     // 3: Default mode: Startup, shutdown, significant operational events (e.g., config reload)
	LevelDebug                    // 4: Verbose: All high-volume messages (skip, match, reset, cleanup, watch polling)
)

var CurrentLogLevel = LevelWarning // Default level set to WARNING
var LogLevelMap = map[string]LogLevel{
	"critical": LevelCritical,
	"error":    LevelError,
	"warning":  LevelWarning,
	"info":     LevelInfo,
	"debug":    LevelDebug,
}

// --- IP VERSION CONSTANTS ---
const (
	VersionInvalid = "invalid"
	VersionIPv4    = "ipv4"
	VersionIPv6    = "ipv6"
)

// --- YAML DATA STRUCTURES ---

type StepDefYAML struct {
	Order           int               `yaml:"order"`
	FieldMatches    map[string]string `yaml:"field_matches"`
	MaxDelaySeconds string            `yaml:"max_delay"`
	MinDelaySeconds string            `yaml:"min_delay"`
}

type BehavioralChainYAML struct {
	Name          string        `yaml:"name"`
	Steps         []StepDefYAML `yaml:"steps"`
	Action        string        `yaml:"action"`
	BlockDuration string        `yaml:"block_duration"`
	MatchKey      string        `yaml:"match_key"`
}

type ChainConfig struct {
	Version        string                `yaml:"version"`
	Chains         []BehavioralChainYAML `yaml:"chains"`
	WhitelistCIDRs []string              `yaml:"whitelist_cidrs"`
}

// --- RUNTIME DATA STRUCTURES ---

type LogEntry struct {
	Timestamp  time.Time // Actual time of the request (parsed from log, not time.Now()).
	IP         string
	Path       string
	Method     string
	Protocol   string
	UserAgent  string
	Referrer   string
	StatusCode int
}

type StepDef struct {
	Order            int
	FieldMatches     map[string]string
	MaxDelayDuration time.Duration
	MinDelayDuration time.Duration
	CompiledRegexes  map[string]*regexp.Regexp // Pre-compiled regexes for performance.
}

type BehavioralChain struct {
	Name          string
	Steps         []StepDef
	Action        string
	BlockDuration time.Duration
	MatchKey      string // (ip, ipv4, ipv6, ip_ua, ipv4_ua, ipv6_ua)
}

type StepState struct {
	CurrentStep   int
	LastMatchTime time.Time
}

// TrackingKey is a comparable struct used as the key for the ActivityStore map.
type TrackingKey struct {
	IP string
	UA string // UserAgent. Empty string if tracking is IP-only.
}

// BotActivity tracks state for a single IP address (or IP+UA combination) across all chains.
type BotActivity struct {
	LastRequestTime time.Time // Time of the IP's PREVIOUS overall request.
	ChainProgress   map[string]StepState
	IsBlocked       bool      // Flag to skip chain checks if this key is blocked.
	BlockedUntil    time.Time // Time when the block expires.
}

// --- GLOBAL STATE ---
var (
	ActivityStore = make(map[TrackingKey]*BotActivity)
	ActivityMutex sync.Mutex // Mutex protecting concurrent access to ActivityStore.

	Chains      []BehavioralChain
	ChainMutex  sync.RWMutex
	LastModTime time.Time

	WhitelistNets []*net.IPNet // Holds parsed CIDR networks

	DryRunActivityStore = make(map[TrackingKey]*BotActivity)
	DryRunActivityMutex sync.Mutex
)

// --- IP/LOGIC HELPERS ---

// GetIPVersion returns the version of the IP address string ("ipv4", "ipv6", or "invalid").
func GetIPVersion(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return VersionInvalid
	}
	if ip.To4() != nil {
		return VersionIPv4
	}
	if len(ip) == net.IPv6len {
		return VersionIPv6
	}
	return VersionInvalid
}

// IsIPWhitelisted checks if the given IP address falls within any configured CIDR whitelist range.
func IsIPWhitelisted(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	// Note: WhitelistNets is protected by ChainMutex because it's populated during config reload.
	ChainMutex.RLock()
	defer ChainMutex.RUnlock()

	for _, ipNet := range WhitelistNets {
		if ipNet.Contains(ip) {
			return true
		}
	}
	return false
}

// GetTrackingKey generates the unique state-tracking key based on the chain's configuration.
func GetTrackingKey(chain *BehavioralChain, entry *LogEntry) TrackingKey {
	ipVersion := GetIPVersion(entry.IP)
	trackingKey := TrackingKey{IP: entry.IP}

	switch chain.MatchKey {
	case "ip", "ip_ua":
		if ipVersion == VersionInvalid {
			return TrackingKey{}
		}
	case "ipv4", "ipv4_ua":
		if ipVersion != VersionIPv4 {
			return TrackingKey{}
		}
	case "ipv6", "ipv6_ua":
		if ipVersion != VersionIPv6 {
			return TrackingKey{}
		}
	default:
		return TrackingKey{}
	}

	if strings.HasSuffix(chain.MatchKey, "_ua") {
		trackingKey.UA = entry.UserAgent
	}

	return trackingKey
}

// --- CLI FLAG INITIALIZATION AND PARSING ---

func init() {
	flag.StringVar(&LogFilePath, "log-path", "/var/log/http/access.log", "Path to the live access log file to tail (ignored in dry-run).")
	flag.StringVar(&HAProxySocketPath, "socket-path", "/var/run/haproxy.sock", "Path to the HAProxy Runtime API Unix socket (ignored in dry-run).")
	flag.StringVar(&BlockedMapPath, "map-path", "/etc/haproxy/maps/blocked_ips.map", "Path to the HAProxy map file used for dynamic IP blocking (ignored in dry-run).")

	flag.StringVar(&YAMLFilePath, "yaml-path", "chains.yaml", "Path to the YAML configuration file defining behavioral chains.")
	flag.StringVar(&PollingIntervalStr, "poll-interval", "5s", "Interval (e.g., '10s', '1m') to check the YAML file for changes (ignored in dry-run).")

	flag.StringVar(&CleanupIntervalStr, "cleanup-interval", "1m", "Interval (e.g., '5m') to run the routine that cleans up idle IP state.")
	flag.StringVar(&IdleTimeoutStr, "idle-timeout", "30m", "Duration (e.g., '45m') an IP must be inactive before its state is purged from memory.")

	flag.StringVar(&LogLevelStr, "log-level", "warning", "Set minimum log level to display: critical, error, warning, info, debug.")
	flag.BoolVar(&DryRun, "dry-run", false, "If true, runs in test mode: skips HAProxy/live logging, ignores cleanup/polling, and uses --test-log.")
	flag.StringVar(&TestLogPath, "test-log", "test_access.log", "Path to a static file containing log lines for dry-run testing.")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "A behavioral bot detection tool that monitors logs and blocks malicious IPs via the HAProxy Runtime API.\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nMemory and CPU are optimized by pre-compiling regexes and using the cleanup routine.\n")
	}
}

// LogOutput checks the level against the configured CurrentLogLevel and prints the message if appropriate.
func LogOutput(level LogLevel, prefix string, format string, v ...interface{}) {
	if level <= CurrentLogLevel {
		log.Printf("[%s] "+format, append([]interface{}{prefix}, v...)...)
	}
}

func ParseDurations() error {
	var err error

	if level, ok := LogLevelMap[strings.ToLower(LogLevelStr)]; ok {
		CurrentLogLevel = level
	} else {
		return fmt.Errorf("invalid log-level '%s'. Must be one of: critical, error, warning, info, debug", LogLevelStr)
	}

	if !DryRun {
		PollingInterval, err = time.ParseDuration(PollingIntervalStr)
		if err != nil {
			return fmt.Errorf("invalid poll-interval format: %w", err)
		}
		CleanupInterval, err = time.ParseDuration(CleanupIntervalStr)
		if err != nil {
			return fmt.Errorf("invalid cleanup-interval format: %w", err)
		}
		IdleTimeout, err = time.ParseDuration(IdleTimeoutStr)
		if err != nil {
			return fmt.Errorf("invalid idle-timeout format: %w", err)
		}
	}
	return nil
}

// --- HAProxy BLOCKING FUNCTION ---

// BlockIPForDuration sends a block command to the HAProxy socket and checks the response.
func BlockIPForDuration(ip string, duration time.Duration) error {
	if DryRun {
		LogOutput(LevelInfo, "DRYRUN", "Would block IP %s for %v (Chain complete).", ip, duration)
		return nil
	}

	haproxyDuration := fmt.Sprintf("%.0fs", duration.Seconds())
	command := fmt.Sprintf("set map %s %s true timeout %s\n", BlockedMapPath, ip, haproxyDuration)

	conn, err := net.Dial("unix", HAProxySocketPath)
	if err != nil {
		LogOutput(LevelError, "ERROR", "Failed to connect to HAProxy socket %s during block attempt for IP %s: %v", HAProxySocketPath, ip, err)
		LogOutput(LevelWarning, "FAILSAFE", "Block for IP %s downgraded to LOG action.", ip)
		return nil
	}
	defer conn.Close()

	if _, err = conn.Write([]byte(command)); err != nil {
		LogOutput(LevelError, "ERROR", "Failed to send command to HAProxy for IP %s: %v", ip, err)
		return nil
	}

	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')

	if err != nil && !errors.Is(err, io.EOF) {
		LogOutput(LevelError, "ERROR", "HAProxy response read error for IP %s: %v", ip, err)
		return nil
	}

	trimmedResponse := strings.TrimSpace(response)

	if strings.HasPrefix(trimmedResponse, "500") || strings.Contains(trimmedResponse, "error") {
		LogOutput(LevelError, "HAPROXY_ERR", "HAProxy execution failed for IP %s. Response: %s", ip, trimmedResponse)
		return nil
	}

	LogOutput(LevelCritical, "HAPROXY_BLOCK", "IP %s blocked for %v (via map: %s)", ip, duration, BlockedMapPath)
	return nil
}

// --- MEMORY LEAK PREVENTION ROUTINE ---

// CleanUpIdleActivity periodically purges state for IPs inactive longer than IdleTimeout.
func CleanUpIdleActivity() {
	if DryRun {
		return
	}

	LogOutput(LevelDebug, "CLEANUP", "Starting Cleanup routine. Purging state older than %v every %v.", IdleTimeout, CleanupInterval)
	for {
		time.Sleep(CleanupInterval)

		ActivityMutex.Lock()
		now := time.Now()
		deletedCount := 0

		for trackingKey, activity := range ActivityStore {
			if now.Sub(activity.LastRequestTime) > IdleTimeout {
				if trackingKey.UA != "" {
					LogOutput(LevelDebug, "CLEANUP", "Purging idle key: %s (UA: %s)", trackingKey.IP, trackingKey.UA)
				} else {
					LogOutput(LevelDebug, "CLEANUP", "Purging idle IP: %s", trackingKey.IP)
				}
				delete(ActivityStore, trackingKey)
				deletedCount++
			}
		}
		ActivityMutex.Unlock()

		if deletedCount > 0 {
			LogOutput(LevelDebug, "CLEANUP", "Complete: Purged %d idle IP states. Current active keys: %d", deletedCount, len(ActivityStore))
		}
	}
}

// --- YAML LOADING & WATCHER LOGIC ---

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

// --- CORE BEHAVIORAL ANALYSIS ---

// New helper: non-locking variant used when caller already holds the mutex.
func GetOrCreateActivityUnsafe(store map[TrackingKey]*BotActivity, trackingKey TrackingKey) *BotActivity {
	if activity, exists := store[trackingKey]; exists {
		return activity
	}
	newActivity := &BotActivity{
		LastRequestTime: time.Time{},
		ChainProgress:   make(map[string]StepState),
	}
	store[trackingKey] = newActivity
	return newActivity
}

// GetOrCreateActivity retrieves or initializes a BotActivity struct for a given tracking key, ensuring thread safety.
func GetOrCreateActivity(trackingKey TrackingKey) *BotActivity {
	store := ActivityStore
	mutex := &ActivityMutex

	if DryRun {
		store = DryRunActivityStore
		mutex = &DryRunActivityMutex
	}

	mutex.Lock()
	defer mutex.Unlock()

	return GetOrCreateActivityUnsafe(store, trackingKey)
}

// GetMatchValue retrieves the field value from a LogEntry based on the field name.
func GetMatchValue(fieldName string, entry *LogEntry) (string, error) {
	switch fieldName {
	case "IP":
		return entry.IP, nil
	case "Path":
		return entry.Path, nil
	case "Method":
		return entry.Method, nil
	case "Protocol":
		return entry.Protocol, nil
	case "UserAgent":
		return entry.UserAgent, nil
	case "Referrer":
		return entry.Referrer, nil
	case "StatusCode":
		return strconv.Itoa(entry.StatusCode), nil
	default:
		return "", fmt.Errorf("unknown field: %s", fieldName)
	}
}

// CheckChains iterates through all chains and updates the IP's progress.
func CheckChains(entry *LogEntry) {

	ChainMutex.RLock()
	currentChains := Chains
	ChainMutex.RUnlock()

	for _, chain := range currentChains {

		trackingKey := GetTrackingKey(&chain, entry)

		if trackingKey == (TrackingKey{}) {
			LogOutput(LevelDebug, "SKIP", "IP %s: Skipped chain '%s'. IP version does not match required key type (%s).", entry.IP, chain.Name, chain.MatchKey)
			continue
		}

		// Choose appropriate store & mutex based on mode.
		store := ActivityStore
		mutex := &ActivityMutex
		if DryRun {
			store = DryRunActivityStore
			mutex = &DryRunActivityMutex
		}

		// Lock once for operations that need atomic access.
		mutex.Lock()

		// Use unsafe variant since we already hold the mutex.
		activity := GetOrCreateActivityUnsafe(store, trackingKey)

		state, exists := activity.ChainProgress[chain.Name]
		if !exists {
			state = StepState{}
		}

		nextStepIndex := state.CurrentStep
		if nextStepIndex >= len(chain.Steps) {
			nextStepIndex = 0
		}

		var nextStep StepDef
		if nextStepIndex < len(chain.Steps) {
			nextStep = chain.Steps[nextStepIndex]
		}

		// 1. Minimum Delay Check (min_delay)
		if nextStep.MinDelayDuration > 0 {
			var timeSource time.Time

			if state.CurrentStep == 0 {
				ipOnlyKey := TrackingKey{IP: entry.IP, UA: ""}
				// Use unsafe access to avoid re-locking the same mutex.
				ipActivity := GetOrCreateActivityUnsafe(store, ipOnlyKey)
				timeSource = ipActivity.LastRequestTime
			} else {
				timeSource = state.LastMatchTime
			}

			if !timeSource.IsZero() {
				timeSinceLastHit := entry.Timestamp.Sub(timeSource)

				// Only skip if the time difference is > 0 but < min_delay.
				// This allows hits logged in the same second (0s difference) to proceed.
				if timeSinceLastHit > 0 && timeSinceLastHit < nextStep.MinDelayDuration {
					LogOutput(LevelDebug, "SKIP", "IP %s: Hit for step %d of chain '%s' skipped (delay too short: %v < %v).", entry.IP, nextStepIndex+1, chain.Name, timeSinceLastHit, nextStep.MinDelayDuration)
					mutex.Unlock()
					continue
				}
			}
		}

		// 2. Maximum Delay Check: Reset progress if delay exceeds the Max Delay Duration.
		if state.CurrentStep > 0 && nextStepIndex < len(chain.Steps) {
			prevStep := chain.Steps[state.CurrentStep-1]
			delay := entry.Timestamp.Sub(state.LastMatchTime)

			if prevStep.MaxDelayDuration > 0 && delay > prevStep.MaxDelayDuration {
				LogOutput(LevelDebug, "RESET", "IP %s: Progress on step %d of chain '%s' reset due to max_delay timeout (%v > %v).", entry.IP, state.CurrentStep, chain.Name, delay, prevStep.MaxDelayDuration)
				state.CurrentStep = 0
				nextStepIndex = 0
				nextStep = chain.Steps[nextStepIndex]
			}
		}

		// 3. Field Match Check
		allFieldsMatch := false
		if nextStepIndex < len(chain.Steps) {
			allFieldsMatch = true

			for fieldName := range nextStep.FieldMatches {
				regex := nextStep.CompiledRegexes[fieldName]
				fieldValue := ""
				var err error

				switch fieldName {
				case "Referrer":
					fieldValue = entry.Referrer
				case "ReferrerPrevPath":
					if entry.Referrer != "" {
						u, parseErr := url.Parse(entry.Referrer)
						if parseErr == nil && u.Path != "" {
							fieldValue = u.Path
						} else {
							if !DryRun {
								LogOutput(LevelWarning, "WARN", "Failed to parse URL path from referrer: %s (Error: %v)", entry.Referrer, parseErr)
							}
							fieldValue = entry.Referrer
						}
					} else {
						allFieldsMatch = false
						break
					}
				case "StatusCode":
					fieldValue = strconv.Itoa(entry.StatusCode)
				default:
					fieldValue, err = GetMatchValue(fieldName, entry)
				}

				if err != nil {
					LogOutput(LevelError, "ERROR", "Internal error in GetMatchValue for field %s: %v", fieldName, err)
					allFieldsMatch = false
					break
				}

				if !regex.MatchString(fieldValue) {
					allFieldsMatch = false
					break
				}
			}

			if allFieldsMatch {
				// Corrected Logging Logic
				isCompletion := state.CurrentStep+1 == len(chain.Steps)

				if isCompletion {
					LogOutput(LevelDebug, "MATCH", "IP %s: Matched final step %d of chain '%s'. Chain completion detected.", entry.IP, state.CurrentStep+1, chain.Name)
				} else {
					LogOutput(LevelDebug, "MATCH", "IP %s: Matched step %d of chain '%s'. Progressing to step %d.", entry.IP, state.CurrentStep+1, chain.Name, state.CurrentStep+2)
				}

				state.CurrentStep++
				state.LastMatchTime = entry.Timestamp

				// 4. Check for Chain Completion
				if state.CurrentStep == len(chain.Steps) {
					ipToBlock := entry.IP
					isWhitelisted := IsIPWhitelisted(ipToBlock) // Check whitelist status here

					if chain.Action == "block" {
						baseMessage := fmt.Sprintf("BLOCK! Chain: %s completed by IP %s. Attempting to block for %v.", chain.Name, ipToBlock, chain.BlockDuration)

						if isWhitelisted {
							LogOutput(LevelCritical, "ALERT", "%s (IP is whitelisted: BLOCK ACTION SKIPPED)", baseMessage)
						} else {
							// Original logic for non-whitelisted block: two logs (ALERT + HAPROXY_BLOCK)
							LogOutput(LevelCritical, "ALERT", baseMessage)

							if err := BlockIPForDuration(ipToBlock, chain.BlockDuration); err != nil {
							}

							// Optimization: Mark the IP-ONLY key as blocked for skipping future log lines from this IP.
							ipOnlyKey := TrackingKey{IP: entry.IP, UA: ""}
							ipActivity := GetOrCreateActivityUnsafe(store, ipOnlyKey)
							ipActivity.IsBlocked = true
							ipActivity.BlockedUntil = entry.Timestamp.Add(chain.BlockDuration) // Set block expiration time
						}

					} else if chain.Action == "log" {

						// CONSOLIDATED LOGGING LOGIC for action: log
						baseMessage := fmt.Sprintf("LOG! Chain: %s completed by IP %s. Action set to 'log'.", chain.Name, entry.IP)

						if isWhitelisted {
							// Consolidate into a single log line when whitelisted
							LogOutput(LevelCritical, "ALERT", "%s (IP is whitelisted: NO FURTHER ACTION TAKEN)", baseMessage)
						} else {
							// Original log output for non-whitelisted IPs
							LogOutput(LevelCritical, "ALERT", baseMessage)
						}
					}

					// Reset state *after* action is taken.
					state.CurrentStep = 0
				}
			}
		}

		// 5. Conditional Update and Cleanup of ChainProgress State (Memory Optimization)
		// Only store the state if the key is actively progressing (CurrentStep > 0).
		if state.CurrentStep > 0 {
			activity.ChainProgress[chain.Name] = state
		} else {
			delete(activity.ChainProgress, chain.Name)
		}

		mutex.Unlock()
	}
}

// --- LOG PARSING & TAILING ---

// ParseLogLine processes a raw log line into a LogEntry.
func ParseLogLine(line string) (*LogEntry, error) {
	parts := strings.Split(line, "\"")
	if len(parts) < 6 {
		return nil, fmt.Errorf("malformed log line: expected at least 6 quoted sections (got %d)", len(parts))
	}

	ipPart := strings.Fields(parts[0])
	requestPart := strings.Fields(parts[1])
	statusSizePart := strings.Fields(parts[2])

	// We expect at least 5 fields in ipPart (e.g., 127.0.0.1 - - [time tz]),
	// at least 3 fields in requestPart (e.g., GET /path HTTP/1.1)
	if len(ipPart) < 5 || len(requestPart) < 3 || len(statusSizePart) < 1 {
		return nil, fmt.Errorf("malformed essential fields (missing Protocol, Request, Status, or incomplete Host/Time fields)")
	}

	var ip string
	var timeIndexStart, timeIndexEnd int

	// Determine structure based on field count in the first part:
	if len(ipPart) >= 6 {
		// Format: Hostname IP - - [Time TZ] (e.g., musicbrainz.org 197.3.177.209 ...)
		ip = ipPart[1]
		timeIndexStart = 4
		timeIndexEnd = 5
	} else {
		// Format: IP - - [Time TZ] (e.g., 127.0.0.1 - - ...)
		ip = ipPart[0]
		timeIndexStart = 3
		timeIndexEnd = 4
	}

	method := requestPart[0]
	path := requestPart[1]
	protocol := requestPart[2] // EXTRACT PROTOCOL VERSION
	referrer := parts[3]
	userAgent := parts[5]

	statusCode, err := strconv.Atoi(statusSizePart[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse status code: %w", err)
	}

	timeStrWithBrackets := ipPart[timeIndexStart] + " " + ipPart[timeIndexEnd]
	timeStr := strings.Trim(timeStrWithBrackets, "[]")

	t, parseErr := time.Parse("02/Jan/2006:15:04:05 -0700", timeStr)
	if parseErr != nil {
		LogOutput(LevelWarning, "WARN", "Failed to parse log time '%s'. Using current time: %v", timeStr, parseErr)
		t = time.Now()
	}

	return &LogEntry{
		Timestamp:  t,
		IP:         ip,
		Path:       path,
		Method:     method,
		Protocol:   protocol,
		StatusCode: statusCode,
		Referrer:   referrer,
		UserAgent:  userAgent,
	}, nil
}

// ProcessLogLine processes a single raw log line, handling skipping of empty/comment lines,
// parsing, chain checking, and updating the activity store.
func ProcessLogLine(line string, lineNumber int) {
	// Skip truly empty lines and comments.
	if line == "" || line == "\n" || line == "\r\n" || strings.HasPrefix(line, "#") {
		return
	}

	entry, parseErr := ParseLogLine(line)
	if parseErr != nil {
		logLevel := LevelDebug
		prefix := "PARSE_FAIL"
		if DryRun {
			prefix = "DRYRUN_WARN"
			logLevel = LevelWarning
		}

		lineStart := line
		if len(line) > 60 {
			lineStart = line[:60] + "..."
		}
		LogOutput(logLevel, prefix, "[Line %d] Failed to parse log line: %v (Line start: %s)", lineNumber, parseErr, lineStart)
		return
	}

	// Define the IP-Only key for block status check and last request time update.
	ipOnlyKey := TrackingKey{IP: entry.IP, UA: ""}

	store := ActivityStore
	mutex := &ActivityMutex
	if DryRun {
		store = DryRunActivityStore
		mutex = &DryRunActivityMutex
	}

	// Lock the mutex for atomic access to the store for status check and time updates.
	mutex.Lock()

	// 1. Get/Update IP-only key (for block status check and min_delay Step 1 time).
	ipActivity := GetOrCreateActivityUnsafe(store, ipOnlyKey)
	ipActivity.LastRequestTime = entry.Timestamp

	isBlocked := ipActivity.IsBlocked
	blockedUntil := ipActivity.BlockedUntil

	// Check if block has expired
	if isBlocked && entry.Timestamp.After(blockedUntil) {
		ipActivity.IsBlocked = false
		ipActivity.BlockedUntil = time.Time{}
		isBlocked = false
		LogOutput(LevelDebug, "UNBLOCK_EXPIRY", "IP %s block has expired at %s. Unmarked as blocked.", entry.IP, blockedUntil.Format("2006-01-02 15:04:05 -0700"))
	}

	// Unlock before calling CheckChains, which also locks.
	mutex.Unlock()

	if isBlocked {
		// Log the skip and return immediately. This is the IP-only optimization.
		LogOutput(LevelDebug, "SKIP", "IP %s is currently marked as blocked until %s. Skipping chain checks.", entry.IP, blockedUntil.Format("2006-01-02 15:04:05 -0700"))
		return
	}

	// Only proceed to check chains if the IP is not blocked.
	CheckChains(entry)
}

// --- CORE FILE I/O IMPLEMENTATION ---

// ReadLineWithLimit reads a line from the bufio.Reader until a newline is found or the limit is hit.
// It uses r.Read(b) for robust final EOF detection, preventing the hang when reading a static file.
func ReadLineWithLimit(r *bufio.Reader, maxBytes int) (string, error) {
	var lineBuilder strings.Builder
	bytesRead := 0

	// Use a 1-byte buffer to read instead of ReadByte().
	b := make([]byte, 1)

	for {
		n, err := r.Read(b)

		if err != nil {
			if n > 0 {
				lineBuilder.Write(b[:n])
			}
			return lineBuilder.String(), err
		}

		// n will always be 1 here for a successful read
		char := b[0]

		if char == '\n' {
			return lineBuilder.String(), nil // Full line found
		}

		lineBuilder.WriteByte(char)
		bytesRead++

		if bytesRead > maxBytes {
			// CRITICAL LIMIT HIT. Drain the rest of the line until the next \n.
			for {
				b_drain, err := r.ReadByte()
				if err != nil {
					// We hit EOF or an I/O error while draining.
					return "", fmt.Errorf("%w (draining failed with error: %v)", ErrLineSkipped, err)
				}
				if b_drain == '\n' {
					break // Successfully drained the rest of the line.
				}
			}
			// Return the skip error. The reader is now correctly positioned at the start of the next line.
			return "", ErrLineSkipped
		}
	}
}

// DryRunLogProcessor reads and processes a static log file for testing.
func DryRunLogProcessor(done chan<- struct{}) {
	LogOutput(LevelInfo, "DRYRUN", "MODE: Reading test logs from %s...", TestLogPath)

	file, err := os.Open(TestLogPath)
	if err != nil {
		log.Fatalf("[FATAL] Dry Run Failed: Could not open test log file %s: %v", TestLogPath, err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineNumber := 0

	for {
		lineNumber++

		line, err := ReadLineWithLimit(reader, MaxLogLineSize)

		if err != nil {
			if errors.Is(err, io.EOF) {
				// Process final line fragment if present
				if line != "" {
					ProcessLogLine(line, lineNumber)
				}
				break
			}
			if errors.Is(err, ErrLineSkipped) {
				LogOutput(LevelWarning, "SKIPPED", "Line %d exceeded critical limit and was skipped.", lineNumber)
				lineNumber--
				continue
			}

			LogOutput(LevelError, "DRYRUN_ERROR", "Reading log file: %v. Exiting dry-run loop.", err)
			break
		}

		ProcessLogLine(line, lineNumber)
	}

	LogOutput(LevelInfo, "DRYRUN", "COMPLETED: Processed all lines in test log.")
	LogOutput(LevelDebug, "DRYRUN", "Total lines processed: %d", lineNumber)

	close(done)
}

// RunDryRun orchestrates the dry-run and manages graceful shutdown.
func RunDryRun() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// Channel to signal completion of log processing.
	done := make(chan struct{})

	go func() {
		DryRunLogProcessor(done)
	}()

	LogOutput(LevelInfo, "INFO", "Running in Dry-Run Mode. Log level set to %s. Log line critical limit: %dKB.", strings.ToUpper(LogLevelStr), MaxLogLineSize/1024)

	select {
	case <-done:
		LogOutput(LevelInfo, "DRYRUN", "COMPLETE: Dry-run successfully finished processing log file.")
		LogOutput(LevelInfo, "INFO", "Total distinct IP/UA keys processed: %d", len(DryRunActivityStore))
		LogOutput(LevelCritical, "SHUTDOWN", "Dry-run complete. Exiting.")
	case <-stop:
		LogOutput(LevelCritical, "SHUTDOWN", "Interrupt signal received during dry-run. Shutting down...")
	}

	os.Exit(0)
}

// TailLogWithRotation tails a log file indefinitely, supporting rotation via inode checks.
func TailLogWithRotation() {
	if DryRun {
		return
	}

	LogOutput(LevelInfo, "TAIL", "Starting live log tailing on %s with rotation support...", LogFilePath)

	for {
		file, err := os.OpenFile(LogFilePath, os.O_RDONLY, 0644)
		if err != nil {
			LogOutput(LevelError, "TAIL_ERROR", "Failed to open log file %s: %v. Retrying in 5s.", LogFilePath, err)
			time.Sleep(5 * time.Second)
			continue
		}

		// Seek to the end of the file
		_, err = file.Seek(0, 2)
		if err != nil {
			LogOutput(LevelError, "TAIL_ERROR", "Failed to seek to end of log file: %v. Closing and retrying.", err)
			file.Close()
			time.Sleep(1 * time.Second)
			continue
		}

		initialStat, err := file.Stat()
		if err != nil {
			LogOutput(LevelError, "TAIL_ERROR", "Failed to stat open file: %v. Closing and retrying.", err)
			file.Close()
			time.Sleep(1 * time.Second)
			continue
		}
		// On Linux/Unix, Stat().Sys() returns *syscall.Stat_t
		initialSysStat := initialStat.Sys().(*syscall.Stat_t)
		initialDev := initialSysStat.Dev
		initialIno := initialSysStat.Ino

		reader := bufio.NewReader(file)
		LogOutput(LevelInfo, "TAIL", "Now tailing (Dev: %d, Inode: %d)", initialDev, initialIno)

		lineNumber := 0

		for {
			lineNumber++

			line, err := ReadLineWithLimit(reader, MaxLogLineSize)
			finalErr := err

			if errors.Is(finalErr, ErrLineSkipped) {
				LogOutput(LevelWarning, "SKIPPED", "Live log line exceeded critical limit of %dKB and was skipped.", MaxLogLineSize/1024)
				continue
			}

			if finalErr != nil {
				if finalErr == io.EOF { // Standard check for live tail: sleep and check rotation
					currentStat, statErr := os.Stat(LogFilePath)
					if statErr == nil {
						if currentStat.Size() < initialStat.Size() {
							LogOutput(LevelDebug, "TAIL", "Detected log file size reduction (truncation/rotation). Reopening file.")
							file.Close()
							break
						}
						currentSysStat := currentStat.Sys().(*syscall.Stat_t)
						if currentSysStat.Dev != initialDev || currentSysStat.Ino != initialIno {
							LogOutput(LevelInfo, "TAIL", "Detected log file rotation (Inode changed from %d to %d). Reopening file.", initialIno, currentSysStat.Ino)
							file.Close()
							break
						}
					} else {
						LogOutput(LevelError, "TAIL_ERROR", "Failed to stat log path during EOF check: %v. Reopening in 1s.", statErr)
						time.Sleep(1 * time.Second)
						file.Close()
						break
					}
					time.Sleep(200 * time.Millisecond)
					continue
				} else {
					LogOutput(LevelError, "TAIL_ERROR", "Reading log file: %v. Reopening in 1s.", finalErr)
					time.Sleep(1 * time.Second)
					file.Close()
					break
				}
			}

			ProcessLogLine(line, lineNumber)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// --- MAIN FUNCTION ---

func main() {
	flag.Parse()

	if err := ParseDurations(); err != nil {
		log.Fatalf("[FATAL] Configuration Error: %v", err)
	}

	var err error
	Chains, err = LoadChainsFromYAML()
	if err != nil {
		log.Fatalf("[FATAL] Initial chain load failed: %v", err)
	}
	LogOutput(LevelInfo, "LOAD", "Initial configuration loaded. Loaded %d behavioral chains.", len(Chains))

	if DryRun {
		RunDryRun()
		return
	} else {
		LogOutput(LevelInfo, "INFO", "Running in Production Mode with per-attempt HAProxy Fail-Safe. Log level set to %s. Log line critical limit: %dKB.", strings.ToUpper(LogLevelStr), MaxLogLineSize/1024)

		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

		if fileInfo, err := os.Stat(YAMLFilePath); err == nil {
			LastModTime = fileInfo.ModTime()
		}

		go ChainWatcher()
		go CleanUpIdleActivity()
		go TailLogWithRotation()

		<-stop
		LogOutput(LevelCritical, "SHUTDOWN", "Interrupt signal received. Shutting down gracefully...")
		LogOutput(LevelCritical, "SHUTDOWN", "Exiting.")
	}
}
