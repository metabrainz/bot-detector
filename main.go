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
var errLineSkipped = errors.New("line exceeded critical limit and was skipped")

// --- CONFIGURATION GLOBAL VARS (Set by CLI flags) ---
var (
	logFilePath       string
	haproxySocketPath string
	blockedMapPath    string

	yamlFilePath       string
	pollingIntervalStr string

	cleanupIntervalStr string
	idleTimeoutStr     string // Duration an IP must be inactive before its state is purged.

	logLevelStr string
	dryRun      bool
	testLogPath string

	pollingInterval time.Duration
	cleanupInterval time.Duration
	idleTimeout     time.Duration
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

var currentLogLevel = LevelWarning // Default level set to WARNING
var logLevelMap = map[string]LogLevel{
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
	Version string                `yaml:"version"`
	Chains  []BehavioralChainYAML `yaml:"chains"`
}

// --- RUNTIME DATA STRUCTURES ---

type LogEntry struct {
	Timestamp  time.Time // Actual time of the request (parsed from log, not time.Now()).
	IP         string
	Path       string
	Method     string
	UserAgent  string
	Referrer   string
	StatusCode int
}

type StepDef struct {
	Order            int
	FieldMatches     map[string]string
	MaxDelayDuration time.Duration
	MinDelayDuration time.Duration
	compiledRegexes  map[string]*regexp.Regexp // Pre-compiled regexes for performance.
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
}

// --- GLOBAL STATE ---
var (
	ActivityStore = make(map[TrackingKey]*BotActivity)
	ActivityMutex sync.Mutex // Mutex protecting concurrent access to ActivityStore.

	Chains      []BehavioralChain
	ChainMutex  sync.RWMutex
	lastModTime time.Time

	dryRunActivityStore = make(map[TrackingKey]*BotActivity)
	dryRunActivityMutex sync.Mutex
)

// --- IP/LOGIC HELPERS ---

// getIPVersion returns the version of the IP address string ("ipv4", "ipv6", or "invalid").
func getIPVersion(ipStr string) string {
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

// getTrackingKey generates the unique state-tracking key based on the chain's configuration.
func getTrackingKey(chain *BehavioralChain, entry *LogEntry) TrackingKey {
	ipVersion := getIPVersion(entry.IP)
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
	flag.StringVar(&logFilePath, "log-path", "/var/log/http/access.log", "Path to the live access log file to tail (ignored in dry-run).")
	flag.StringVar(&haproxySocketPath, "socket-path", "/var/run/haproxy.sock", "Path to the HAProxy Runtime API Unix socket (ignored in dry-run).")
	flag.StringVar(&blockedMapPath, "map-path", "/etc/haproxy/maps/blocked_ips.map", "Path to the HAProxy map file used for dynamic IP blocking (ignored in dry-run).")

	flag.StringVar(&yamlFilePath, "yaml-path", "chains.yaml", "Path to the YAML configuration file defining behavioral chains.")
	flag.StringVar(&pollingIntervalStr, "poll-interval", "5s", "Interval (e.g., '10s', '1m') to check the YAML file for changes (ignored in dry-run).")

	flag.StringVar(&cleanupIntervalStr, "cleanup-interval", "1m", "Interval (e.g., '5m') to run the routine that cleans up idle IP state.")
	flag.StringVar(&idleTimeoutStr, "idle-timeout", "30m", "Duration (e.g., '45m') an IP must be inactive before its state is purged from memory.")

	flag.StringVar(&logLevelStr, "log-level", "warning", "Set minimum log level to display: critical, error, warning, info, debug.")
	flag.BoolVar(&dryRun, "dry-run", false, "If true, runs in test mode: skips HAProxy/live logging, ignores cleanup/polling, and uses --test-log.")
	flag.StringVar(&testLogPath, "test-log", "test_access.log", "Path to a static file containing log lines for dry-run testing.")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "A behavioral bot detection tool that monitors logs and blocks malicious IPs via the HAProxy Runtime API.\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nMemory and CPU are optimized by pre-compiling regexes and using the cleanup routine.\n")
	}
}

// logOutput checks the level against the configured currentLogLevel and prints the message if appropriate.
func logOutput(level LogLevel, prefix string, format string, v ...interface{}) {
	if level <= currentLogLevel {
		log.Printf("[%s] "+format, append([]interface{}{prefix}, v...)...)
	}
}

func parseDurations() error {
	var err error

	if level, ok := logLevelMap[strings.ToLower(logLevelStr)]; ok {
		currentLogLevel = level
	} else {
		return fmt.Errorf("invalid log-level '%s'. Must be one of: critical, error, warning, info, debug", logLevelStr)
	}

	if !dryRun {
		pollingInterval, err = time.ParseDuration(pollingIntervalStr)
		if err != nil {
			return fmt.Errorf("invalid poll-interval format: %w", err)
		}
		cleanupInterval, err = time.ParseDuration(cleanupIntervalStr)
		if err != nil {
			return fmt.Errorf("invalid cleanup-interval format: %w", err)
		}
		idleTimeout, err = time.ParseDuration(idleTimeoutStr)
		if err != nil {
			return fmt.Errorf("invalid idle-timeout format: %w", err)
		}
	}
	return nil
}

// --- HAProxy BLOCKING FUNCTION ---

// BlockIPForDuration sends a block command to the HAProxy socket and checks the response.
func BlockIPForDuration(ip string, duration time.Duration) error {
	if dryRun {
		logOutput(LevelInfo, "DRYRUN", "Would block IP %s for %v (Chain complete).", ip, duration)
		return nil
	}

	haproxyDuration := fmt.Sprintf("%.0fs", duration.Seconds())
	command := fmt.Sprintf("set map %s %s true timeout %s\n", blockedMapPath, ip, haproxyDuration)

	conn, err := net.Dial("unix", haproxySocketPath)
	if err != nil {
		logOutput(LevelError, "ERROR", "Failed to connect to HAProxy socket %s during block attempt for IP %s: %v", haproxySocketPath, ip, err)
		logOutput(LevelWarning, "FAILSAFE", "Block for IP %s downgraded to LOG action.", ip)
		return nil
	}
	defer conn.Close()

	if _, err = conn.Write([]byte(command)); err != nil {
		logOutput(LevelError, "ERROR", "Failed to send command to HAProxy for IP %s: %v", ip, err)
		return nil
	}

	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')

	if err != nil && err.Error() != "EOF" {
		logOutput(LevelError, "ERROR", "HAProxy response read error for IP %s: %v", ip, err)
		return nil
	}

	trimmedResponse := strings.TrimSpace(response)

	if strings.HasPrefix(trimmedResponse, "500") || strings.Contains(trimmedResponse, "error") {
		logOutput(LevelError, "HAPROXY_ERR", "HAProxy execution failed for IP %s. Response: %s", ip, trimmedResponse)
		return nil
	}

	logOutput(LevelCritical, "HAPROXY_BLOCK", "IP %s blocked for %v (via map: %s)", ip, duration, blockedMapPath)
	return nil
}

// --- MEMORY LEAK PREVENTION ROUTINE ---

// CleanUpIdleActivity periodically purges state for IPs inactive longer than idleTimeout.
func CleanUpIdleActivity() {
	if dryRun {
		return
	}

	logOutput(LevelDebug, "CLEANUP", "Starting Cleanup routine. Purging state older than %v every %v.", idleTimeout, cleanupInterval)
	for {
		time.Sleep(cleanupInterval)

		ActivityMutex.Lock()
		now := time.Now()
		deletedCount := 0

		for trackingKey, activity := range ActivityStore {
			if now.Sub(activity.LastRequestTime) > idleTimeout {
				if trackingKey.UA != "" {
					logOutput(LevelDebug, "CLEANUP", "Purging idle key: %s (UA: %s)", trackingKey.IP, trackingKey.UA)
				} else {
					logOutput(LevelDebug, "CLEANUP", "Purging idle IP: %s", trackingKey.IP)
				}
				delete(ActivityStore, trackingKey)
				deletedCount++
			}
		}
		ActivityMutex.Unlock()

		if deletedCount > 0 {
			logOutput(LevelDebug, "CLEANUP", "Complete: Purged %d idle IP states. Current active keys: %d", deletedCount, len(ActivityStore))
		}
	}
}

// --- YAML LOADING & WATCHER LOGIC ---

// loadChainsFromYAML reads, parses, and pre-compiles regexes for the chains.
func loadChainsFromYAML() ([]BehavioralChain, error) {
	data, err := os.ReadFile(yamlFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read YAML file %s: %w", yamlFilePath, err)
	}

	var config ChainConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal YAML: %w", err)
	}

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
				compiledRegexes: make(map[string]*regexp.Regexp),
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
				runtimeStep.compiledRegexes[field] = re
			}
			runtimeChain.Steps = append(runtimeChain.Steps, runtimeStep)
		}
		newChains = append(newChains, runtimeChain)
	}

	return newChains, nil
}

// ChainWatcher monitors the YAML config file for modifications and reloads the chains dynamically.
func ChainWatcher() {
	if dryRun {
		return
	}
	logOutput(LevelDebug, "WATCH", "Starting ChainWatcher, polling %s every %v", yamlFilePath, pollingInterval)
	for {
		time.Sleep(pollingInterval)

		fileInfo, err := os.Stat(yamlFilePath)
		if err != nil {
			logOutput(LevelError, "WATCH_ERROR", "Failed to stat file %s: %v", yamlFilePath, err)
			continue
		}
		modTime := fileInfo.ModTime()

		if modTime.After(lastModTime) {
			logOutput(LevelInfo, "WATCH", "Detected change in chains.yaml. Attempting reload...")

			newChains, err := loadChainsFromYAML()
			if err != nil {
				logOutput(LevelError, "LOAD_ERROR", "Failed to reload chains: %v. Retaining previous configuration.", err)
				continue
			}

			ChainMutex.Lock()
			Chains = newChains
			lastModTime = modTime
			ChainMutex.Unlock()
			logOutput(LevelInfo, "LOAD", "Successfully reloaded and compiled %d behavioral chains.", len(newChains))
		}
	}
}

// --- CORE BEHAVIORAL ANALYSIS ---

// New helper: non-locking variant used when caller already holds the mutex.
func getOrCreateActivityUnsafe(store map[TrackingKey]*BotActivity, trackingKey TrackingKey) *BotActivity {
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

	if dryRun {
		store = dryRunActivityStore
		mutex = &dryRunActivityMutex
	}

	mutex.Lock()
	defer mutex.Unlock()

	return getOrCreateActivityUnsafe(store, trackingKey)
}

// getMatchValue retrieves the field value from a LogEntry based on the field name.
func getMatchValue(fieldName string, entry *LogEntry) (string, error) {
	switch fieldName {
	case "IP":
		return entry.IP, nil
	case "Path":
		return entry.Path, nil
	case "Method":
		return entry.Method, nil
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

		trackingKey := getTrackingKey(&chain, entry)

		if trackingKey == (TrackingKey{}) {
			logOutput(LevelDebug, "SKIP", "IP %s: Skipped chain '%s'. IP version does not match required key type (%s).", entry.IP, chain.Name, chain.MatchKey)
			continue
		}

		// Choose appropriate store & mutex based on mode.
		store := ActivityStore
		mutex := &ActivityMutex
		if dryRun {
			store = dryRunActivityStore
			mutex = &dryRunActivityMutex
		}

		// Lock once for operations that need atomic access.
		mutex.Lock()

		// Use unsafe variant since we already hold the mutex.
		activity := getOrCreateActivityUnsafe(store, trackingKey)

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
				ipActivity := getOrCreateActivityUnsafe(store, ipOnlyKey)
				timeSource = ipActivity.LastRequestTime
			} else {
				timeSource = state.LastMatchTime
			}

			if !timeSource.IsZero() {
				timeSinceLastHit := entry.Timestamp.Sub(timeSource)

				if timeSinceLastHit < nextStep.MinDelayDuration {
					logOutput(LevelDebug, "SKIP", "IP %s: Hit for step %d of chain '%s' skipped (delay too short: %v < %v).", entry.IP, nextStepIndex+1, chain.Name, timeSinceLastHit, nextStep.MinDelayDuration)
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
				logOutput(LevelDebug, "RESET", "IP %s: Progress on step %d of chain '%s' reset due to max_delay timeout (%v > %v).", entry.IP, state.CurrentStep+1, chain.Name, delay, prevStep.MaxDelayDuration)
				state.CurrentStep = 0
				nextStepIndex = 0
				nextStep = chain.Steps[nextStepIndex]
			}
		}

		// 3. Field Match Check
		if nextStepIndex < len(chain.Steps) {
			allFieldsMatch := true

			for fieldName := range nextStep.FieldMatches {
				regex := nextStep.compiledRegexes[fieldName]
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
							if !dryRun {
								logOutput(LevelWarning, "WARN", "Failed to parse URL path from referrer: %s (Error: %v)", entry.Referrer, parseErr)
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
					fieldValue, err = getMatchValue(fieldName, entry)
				}

				if err != nil {
					logOutput(LevelError, "ERROR", "Internal error in getMatchValue for field %s: %v", fieldName, err)
					allFieldsMatch = false
					break
				}

				if !regex.MatchString(fieldValue) {
					allFieldsMatch = false
					break
				}
			}

			if allFieldsMatch {
				logOutput(LevelDebug, "MATCH", "IP %s: Matched step %d of chain '%s'. Progressing to step %d.", entry.IP, state.CurrentStep+1, chain.Name, state.CurrentStep+2)
				state.CurrentStep++
				state.LastMatchTime = entry.Timestamp

				// 4. Check for Chain Completion
				if state.CurrentStep == len(chain.Steps) {
					ipToBlock := entry.IP
					if chain.Action == "block" {
						logOutput(LevelCritical, "ALERT", "BLOCK! Chain: %s completed by IP %s. Attempting to block for %v.", chain.Name, ipToBlock, chain.BlockDuration)
						if err := BlockIPForDuration(ipToBlock, chain.BlockDuration); err != nil {
						}
					} else if chain.Action == "log" {
						logOutput(LevelCritical, "ALERT", "LOG! Chain: %s completed by IP %s. Action set to 'log'.", chain.Name, entry.IP)
					}

					state.CurrentStep = 0
				}
			}
		}

		activity.ChainProgress[chain.Name] = state
		mutex.Unlock()
	}
}

// --- LOG PARSING & TAILING ---

// parseLogLine processes a raw log line into a LogEntry.
func parseLogLine(line string) (*LogEntry, error) {
	parts := strings.Split(line, "\"")
	if len(parts) < 6 {
		return nil, fmt.Errorf("malformed log line: expected at least 6 quoted sections (got %d)", len(parts))
	}

	ipPart := strings.Fields(parts[0])
	requestPart := strings.Fields(parts[1])
	statusSizePart := strings.Fields(parts[2])

	if len(ipPart) < 6 || len(requestPart) < 2 || len(statusSizePart) < 1 {
		return nil, fmt.Errorf("malformed essential fields (missing Hostname, Time, Request, or Status)")
	}

	ip := ipPart[1]
	method := requestPart[0]
	path := requestPart[1]
	referrer := parts[3]
	userAgent := parts[5]

	statusCode, err := strconv.Atoi(statusSizePart[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse status code: %w", err)
	}

	timeStrWithBrackets := ipPart[4] + " " + ipPart[5]
	timeStr := strings.Trim(timeStrWithBrackets, "[]")

	t, parseErr := time.Parse("02/Jan/2006:15:04:05 -0700", timeStr)
	if parseErr != nil {
		logOutput(LevelWarning, "WARN", "Failed to parse log time '%s'. Using current time: %v", timeStr, parseErr)
		t = time.Now()
	}

	return &LogEntry{
		Timestamp:  t,
		IP:         ip,
		Path:       path,
		Method:     method,
		StatusCode: statusCode,
		Referrer:   referrer,
		UserAgent:  userAgent,
	}, nil
}

// processLogLine processes a single raw log line, handling skipping of empty/comment lines,
// parsing, chain checking, and updating the activity store.
func processLogLine(line string, lineNumber int) {
	// Skip truly empty lines and comments.
	if line == "" || line == "\n" || line == "\r\n" || strings.HasPrefix(line, "#") {
		return
	}

	entry, parseErr := parseLogLine(line)
	if parseErr != nil {
		logLevel := LevelDebug
		prefix := "PARSE_FAIL"
		if dryRun {
			prefix = "DRYRUN_WARN"
			logLevel = LevelWarning
		}

		lineStart := line
		if len(line) > 60 {
			lineStart = line[:60] + "..."
		}
		logOutput(logLevel, prefix, "[Line %d] Failed to parse log line: %v (Line start: %s)", lineNumber, parseErr, lineStart)
		return
	}

	CheckChains(entry)

	ipOnlyKey := TrackingKey{IP: entry.IP, UA: ""}
	GetOrCreateActivity(ipOnlyKey).LastRequestTime = entry.Timestamp
}

// --- CORE FILE I/O IMPLEMENTATION ---

// readLineWithLimit reads a line from the bufio.Reader until a newline is found or the limit is hit.
// It uses r.Read(b) for robust final EOF detection, preventing the hang when reading a static file.
func readLineWithLimit(r *bufio.Reader, maxBytes int) (string, error) {
	var lineBuilder strings.Builder
	bytesRead := 0

	// Use a 1-byte buffer to read instead of ReadByte().
	// The Read method handles EOF more reliably in this context for static files, resolving the hang.
	b := make([]byte, 1)

	for {
		n, err := r.Read(b)

		if err != nil {
			// If we hit EOF or another error, we return the line fragment read so far (if any).
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
				// Use ReadByte() for draining as its performance is fine here (we are not at EOF).
				b_drain, err := r.ReadByte()
				if err != nil {
					// We hit EOF or an I/O error while draining.
					return "", fmt.Errorf("%w (draining failed with error: %v)", errLineSkipped, err)
				}
				if b_drain == '\n' {
					break // Successfully drained the rest of the line.
				}
			}
			// Return the skip error. The reader is now correctly positioned at the start of the next line.
			return "", errLineSkipped
		}
	}
}

// DryRunLogProcessor reads and processes a static log file for testing.
// This function MUST use a finite loop structure and cleanly exit on io.EOF.
func DryRunLogProcessor(done chan<- struct{}) {
	logOutput(LevelInfo, "DRYRUN", "MODE: Reading test logs from %s...", testLogPath)

	file, err := os.Open(testLogPath)
	if err != nil {
		log.Fatalf("[FATAL] Dry Run Failed: Could not open test log file %s: %v", testLogPath, err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineNumber := 0

	// This is a FINITE loop that MUST exit when the file is read.
	for {
		lineNumber++

		// Assuming readLineWithLimit is still used for line-limit handling.
		line, err := readLineWithLimit(reader, MaxLogLineSize)

		if err != nil {
			if errors.Is(err, io.EOF) {
				// Process final line fragment if present (this ensures files that don't return a newline
				// still have their last line processed).
				if line != "" {
					processLogLine(line, lineNumber)
				}
				break
			}
			if errors.Is(err, errLineSkipped) {
				logOutput(LevelWarning, "SKIPPED", "Line %d exceeded critical limit and was skipped.", lineNumber)
				lineNumber-- // Do not count skipped line against final total
				continue
			}

			// Handle any other I/O errors by logging and exiting the processing loop.
			logOutput(LevelError, "DRYRUN_ERROR", "Reading log file: %v. Exiting dry-run loop.", err)
			break
		}

		// Process the line once it's successfully read.
		processLogLine(line, lineNumber)
	}

	logOutput(LevelInfo, "DRYRUN", "COMPLETED: Processed all lines in test log.")
	logOutput(LevelDebug, "DRYRUN", "Total lines processed: %d", lineNumber)

	// Signal the runDryRun function that processing is complete, allowing the program to exit.
	close(done)
}

// runDryRun orchestrates the dry-run and manages graceful shutdown.
func runDryRun() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// Channel to signal completion of log processing.
	done := make(chan struct{})

	go func() {
		// Pass the done channel to the processor.
		DryRunLogProcessor(done)
	}()

	logOutput(LevelInfo, "INFO", "Running in Dry-Run Mode. Log level set to %s. Log line critical limit: %dKB.", strings.ToUpper(logLevelStr), MaxLogLineSize/1024)

	select {
	case <-done:
		// Program exits here after the goroutine finishes.
		logOutput(LevelInfo, "DRYRUN", "COMPLETE: Dry-run successfully finished processing log file.")
		logOutput(LevelInfo, "INFO", "Total distinct IP/UA keys processed: %d", len(dryRunActivityStore))
		logOutput(LevelCritical, "SHUTDOWN", "Dry-run complete. Exiting.")
	case <-stop:
		// Program exits here if interrupted.
		logOutput(LevelCritical, "SHUTDOWN", "Interrupt signal received during dry-run. Shutting down...")
	}

	// Use os.Exit(0) to forcefully terminate the process and all goroutines cleanly.
	os.Exit(0)
}

// TailLogWithRotation tails a log file indefinitely, supporting rotation via inode checks.
func TailLogWithRotation() {
	if dryRun {
		return
	}

	logOutput(LevelInfo, "TAIL", "Starting live log tailing on %s with rotation support...", logFilePath)

	for {
		file, err := os.OpenFile(logFilePath, os.O_RDONLY, 0644)
		if err != nil {
			logOutput(LevelError, "TAIL_ERROR", "Failed to open log file %s: %v. Retrying in 5s.", logFilePath, err)
			time.Sleep(5 * time.Second)
			continue
		}

		_, err = file.Seek(0, 2)
		if err != nil {
			logOutput(LevelError, "TAIL_ERROR", "Failed to seek to end of log file: %v. Closing and retrying.", err)
			file.Close()
			time.Sleep(1 * time.Second)
			continue
		}

		initialStat, err := file.Stat()
		if err != nil {
			logOutput(LevelError, "TAIL_ERROR", "Failed to stat open file: %v. Closing and retrying.", err)
			file.Close()
			time.Sleep(1 * time.Second)
			continue
		}
		initialSysStat := initialStat.Sys().(*syscall.Stat_t)
		initialDev := initialSysStat.Dev
		initialIno := initialSysStat.Ino

		reader := bufio.NewReader(file)
		logOutput(LevelInfo, "TAIL", "Now tailing (Dev: %d, Inode: %d)", initialDev, initialIno)

		lineNumber := 0

		for {
			lineNumber++

			line, err := readLineWithLimit(reader, MaxLogLineSize)
			finalErr := err

			if errors.Is(finalErr, errLineSkipped) {
				logOutput(LevelWarning, "SKIPPED", "Live log line exceeded critical limit of %dKB and was skipped.", MaxLogLineSize/1024)
				continue
			}

			if finalErr != nil {
				if finalErr == io.EOF { // Standard check for live tail: sleep and check rotation
					currentStat, statErr := os.Stat(logFilePath)
					if statErr == nil {
						if currentStat.Size() < initialStat.Size() {
							logOutput(LevelDebug, "TAIL", "Detected log file size reduction (truncation/rotation). Reopening file.")
							file.Close()
							break
						}
						currentSysStat := currentStat.Sys().(*syscall.Stat_t)
						if currentSysStat.Dev != initialDev || currentSysStat.Ino != initialIno {
							logOutput(LevelInfo, "TAIL", "Detected log file rotation (Inode changed from %d to %d). Reopening file.", initialIno, currentSysStat.Ino)
							file.Close()
							break
						}
					} else {
						logOutput(LevelError, "TAIL_ERROR", "Failed to stat log path during EOF check: %v. Reopening in 1s.", statErr)
						time.Sleep(1 * time.Second)
						file.Close()
						break
					}
					time.Sleep(200 * time.Millisecond)
					continue
				} else {
					logOutput(LevelError, "TAIL_ERROR", "Reading log file: %v. Reopening in 1s.", finalErr)
					time.Sleep(1 * time.Second)
					file.Close()
					break
				}
			}

			processLogLine(line, lineNumber)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// --- MAIN FUNCTION ---

func main() {
	flag.Parse()

	if err := parseDurations(); err != nil {
		log.Fatalf("[FATAL] Configuration Error: %v", err)
	}

	var err error
	Chains, err = loadChainsFromYAML()
	if err != nil {
		log.Fatalf("[FATAL] Initial chain load failed: %v", err)
	}
	logOutput(LevelInfo, "LOAD", "Initial configuration loaded. Loaded %d behavioral chains.", len(Chains))

	if dryRun {
		runDryRun()
		return
	} else {
		logOutput(LevelInfo, "INFO", "Running in Production Mode with per-attempt HAProxy Fail-Safe. Log level set to %s. Log line critical limit: %dKB.", strings.ToUpper(logLevelStr), MaxLogLineSize/1024)

		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

		if fileInfo, err := os.Stat(yamlFilePath); err == nil {
			lastModTime = fileInfo.ModTime()
		}

		go ChainWatcher()
		go CleanUpIdleActivity()
		go TailLogWithRotation()

		<-stop
		logOutput(LevelCritical, "SHUTDOWN", "Interrupt signal received. Shutting down gracefully...")
		logOutput(LevelCritical, "SHUTDOWN", "Exiting.")
	}
}
