package main

import (
	"bufio"
	"flag"
	"fmt"
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

// --- NEW LOGGING STRUCTURE ---
type LogLevel int

const (
	LevelCritical LogLevel = iota // Highest priority: Blocks, Fatal errors
	LevelError                    // Critical failure, but program continues
	LevelWarning                  // Non-critical issues, time parse warnings
	LevelInfo                     // Default mode: Startup, shutdown, significant operational events (e.g., config reload)
	LevelDebug                    // Verbose: All high-volume messages (skip, match, reset, cleanup, watch polling)
)

var currentLogLevel = LevelWarning // Default level set to WARNING
var logLevelMap = map[string]LogLevel{
	"critical": LevelCritical,
	"error":    LevelError,
	"warning":  LevelWarning,
	"info":     LevelInfo,
	"debug":    LevelDebug,
}

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
}

type StepState struct {
	CurrentStep   int
	LastMatchTime time.Time
}

// BotActivity tracks state for a single IP address across all chains.
type BotActivity struct {
	LastRequestTime time.Time // Time of the IP's PREVIOUS overall request (set *after* CheckChains).
	ChainProgress   map[string]StepState
}

// --- GLOBAL STATE ---
var (
	ActivityStore = make(map[string]*BotActivity)
	ActivityMutex sync.Mutex // Mutex protecting concurrent access to ActivityStore.

	Chains      []BehavioralChain
	ChainMutex  sync.RWMutex // Mutex protecting concurrent read/write access during chain reload.
	lastModTime time.Time

	dryRunActivityStore = make(map[string]*BotActivity)
	dryRunActivityMutex sync.Mutex
)

// --- 🧩 CLI FLAG INITIALIZATION AND PARSING ---

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
		// FAIL-SAFE: If connection fails, log error and return nil (downgrade action)
		logOutput(LevelError, "ERROR", "Failed to connect to HAProxy socket %s during block attempt for IP %s: %v", haproxySocketPath, ip, err)
		logOutput(LevelWarning, "FAILSAFE", "Block for IP %s downgraded to LOG action.", ip)
		return nil
	}
	defer conn.Close()

	if _, err = conn.Write([]byte(command)); err != nil {
		return fmt.Errorf("failed to send command to HAProxy: %w", err)
	}

	// Read Response Confirmation
	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')

	if err != nil && err.Error() != "EOF" {
		logOutput(LevelError, "ERROR", "HAProxy response read error for IP %s: %v", ip, err)
		return nil
	}

	trimmedResponse := strings.TrimSpace(response)

	// Display the specific HAProxy error message (e.g., 500 or error keyword).
	if strings.HasPrefix(trimmedResponse, "500") || strings.Contains(trimmedResponse, "error") {
		logOutput(LevelError, "HAPROXY_ERR", "HAProxy execution failed for IP %s. Response: %s", ip, trimmedResponse)
		return nil // Return nil on error for the fail-safe to avoid log noise.
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

		ActivityMutex.Lock() // Protect the global ActivityStore map.
		now := time.Now()
		deletedCount := 0

		for ip, activity := range ActivityStore {
			if now.Sub(activity.LastRequestTime) > idleTimeout {
				delete(ActivityStore, ip)
				deletedCount++
			}
		}
		ActivityMutex.Unlock()

		if deletedCount > 0 {
			logOutput(LevelDebug, "CLEANUP", "Complete: Purged %d idle IP states. Current active IPs: %d", deletedCount, len(ActivityStore))
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

	// Conversion and Pre-Compilation Logic
	for _, yamlChain := range config.Chains {
		runtimeChain := BehavioralChain{Name: yamlChain.Name, Action: yamlChain.Action}
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

			// Compile regexes for performance.
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

// GetOrCreateActivity retrieves or initializes a BotActivity struct for a given IP, ensuring thread safety.
func GetOrCreateActivity(ip string) *BotActivity {
	store := ActivityStore
	mutex := &ActivityMutex

	if dryRun {
		store = dryRunActivityStore
		mutex = &dryRunActivityMutex
	}

	mutex.Lock()
	defer mutex.Unlock()

	if activity, exists := store[ip]; exists {
		// LastRequestTime is intentionally NOT updated here. It is updated *after* CheckChains
		// in the log processor to hold the time of the PREVIOUS request for min_delay checks.
		return activity
	}

	newActivity := &BotActivity{
		LastRequestTime: time.Time{}, // Initialize to zero time to indicate no prior request.
		ChainProgress:   make(map[string]StepState),
	}
	store[ip] = newActivity
	return newActivity
}

// CheckChains iterates through all chains and updates the IP's progress.
func CheckChains(entry *LogEntry) {
	activity := GetOrCreateActivity(entry.IP)

	getMatchValue := func(fieldName string, entry *LogEntry) (string, error) {
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

	ChainMutex.RLock()
	currentChains := Chains
	ChainMutex.RUnlock()

	for _, chain := range currentChains {

		mutex := &ActivityMutex
		if dryRun {
			mutex = &dryRunActivityMutex
		}

		// Lock for the entire state modification to prevent race conditions (Concurrency Fix).
		mutex.Lock()

		state, exists := activity.ChainProgress[chain.Name]
		if !exists {
			state = StepState{}
		}

		nextStepIndex := state.CurrentStep
		if nextStepIndex >= len(chain.Steps) {
			nextStepIndex = 0
		}

		// Identify the step we are attempting to match.
		var nextStep StepDef
		if nextStepIndex < len(chain.Steps) {
			nextStep = chain.Steps[nextStepIndex]
		}

		// 1. Minimum Delay Check (min_delay): Skip processing this log entry for this chain if the delay is too short.
		if nextStep.MinDelayDuration > 0 {
			var timeSource time.Time

			if state.CurrentStep == 0 {
				// First Step: Check min_delay against the IP's global LastRequestTime.
				timeSource = activity.LastRequestTime
			} else {
				// Subsequent Steps: Check min_delay against the chain's last successful step time.
				timeSource = state.LastMatchTime
			}

			// Only check if we have a recorded previous time (i.e., not the absolute first request ever).
			if !timeSource.IsZero() {
				timeSinceLastHit := entry.Timestamp.Sub(timeSource)

				if timeSinceLastHit < nextStep.MinDelayDuration {
					logOutput(LevelDebug, "SKIP", "IP %s: Hit for step %d of chain '%s' skipped (delay too short: %v < %v).", entry.IP, nextStepIndex+1, chain.Name, timeSinceLastHit, nextStep.MinDelayDuration)
					mutex.Unlock() // Release the lock before returning
					return
				}
			}
		}

		// 2. Maximum Delay Check: Reset progress if delay exceeds the Max Delay Duration.
		if state.CurrentStep > 0 && nextStepIndex < len(chain.Steps) {
			prevStep := chain.Steps[state.CurrentStep-1]
			delay := entry.Timestamp.Sub(state.LastMatchTime)

			if prevStep.MaxDelayDuration > 0 && delay > prevStep.MaxDelayDuration {
				// Delay was too long, progress is reset.
				logOutput(LevelDebug, "RESET", "IP %s: Progress on step %d of chain '%s' reset due to max_delay timeout (%v > %v).", entry.IP, state.CurrentStep+1, chain.Name, delay, prevStep.MaxDelayDuration)
				state.CurrentStep = 0 // Reset chain progress
				nextStepIndex = 0
				// Re-fetch the first step for the Field Match Check below
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
					// Extract *only* the path component from the Referrer URL for path-based matching.
					if entry.Referrer != "" {
						u, parseErr := url.Parse(entry.Referrer)
						if parseErr == nil && u.Path != "" {
							fieldValue = u.Path // Use only the path part of the referrer
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
				// Match successful. Advance progress.
				logOutput(LevelDebug, "MATCH", "IP %s: Matched step %d of chain '%s'. Progressing to step %d.", entry.IP, state.CurrentStep+1, chain.Name, state.CurrentStep+2)
				state.CurrentStep++
				state.LastMatchTime = entry.Timestamp

				// 4. Check for Chain Completion
				if state.CurrentStep == len(chain.Steps) {
					if chain.Action == "block" {
						logOutput(LevelCritical, "ALERT", "BLOCK! Chain: %s completed by IP %s. Attempting to block for %v.", chain.Name, entry.IP, chain.BlockDuration)
						if err := BlockIPForDuration(entry.IP, chain.BlockDuration); err != nil {
							// Error is handled/logged in BlockIPForDuration.
						}
					} else if chain.Action == "log" {
						logOutput(LevelCritical, "ALERT", "LOG! Chain: %s completed by IP %s. Action set to 'log'.", chain.Name, entry.IP)
					}

					state.CurrentStep = 0 // Reset chain for potential future attacks.
				}
			}
		}

		// Update the state map (write back the modified local 'state' copy).
		activity.ChainProgress[chain.Name] = state
		mutex.Unlock() // Release the lock.
	}
}

// --- LOG PARSING & TAILING ---

// parseLogLine processes a raw log line into a LogEntry.
func parseLogLine(line string) (*LogEntry, error) {
	// Assumed format: HOSTNAME IP - - [TIME] "METHOD PATH HTTP/1.1" STATUS SIZE "REFERRER" "USERAGENT"
	parts := strings.Split(line, "\"")
	if len(parts) < 5 {
		return nil, fmt.Errorf("malformed log line: too few quoted sections")
	}

	ipPart := strings.Fields(parts[0])
	requestPart := strings.Fields(parts[1])
	statusSizePart := strings.Fields(parts[2])

	// Check for enough fields: Hostname (0), IP (1), ID1 (2), ID2 (3), [TimePart1 (4), TimePart2 (5)]
	if len(ipPart) < 6 || len(requestPart) < 2 || len(statusSizePart) < 1 {
		return nil, fmt.Errorf("malformed essential fields (missing Hostname, Time, Request, or Status)")
	}

	// Correct Indexing for Hostname-prefixed log:
	ip := ipPart[1]
	method := requestPart[0]
	path := requestPart[1]
	referrer := parts[3]
	userAgent := parts[5]

	statusCode, err := strconv.Atoi(statusSizePart[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse status code: %w", err)
	}

	// FIX: Reconstruct the full bracketed timestamp from indices 4 and 5.
	// Example: ipPart[4] = "[28/Oct/2025:15:41:10" and ipPart[5] = "+0000]"
	timeStrWithBrackets := ipPart[4] + " " + ipPart[5]
	timeStr := strings.Trim(timeStrWithBrackets, "[]")

	// Use Go's reference time for layout: "02/Jan/2006:15:04:05 -0700".
	t, parseErr := time.Parse("02/Jan/2006:15:04:05 -0700", timeStr)
	if parseErr != nil {
		// Log warning and use current time if time string is unparseable (e.g., malformed or missing).
		logOutput(LevelWarning, "WARN", "Failed to parse log time '%s'. Using current time: %v", timeStr, parseErr)
		t = time.Now()
	}

	return &LogEntry{
		Timestamp:  t,
		IP:         ip,
		Path:       path,
		Method:     method,
		StatusCode: statusCode,
		Referrer:   strings.TrimSpace(referrer),
		UserAgent:  strings.TrimSpace(userAgent),
	}, nil
}

// DryRunLogProcessor reads and processes a static log file for testing.
func DryRunLogProcessor() {
	logOutput(LevelInfo, "DRYRUN", "MODE: Reading test logs from %s...", testLogPath)

	file, err := os.Open(testLogPath)
	if err != nil {
		log.Fatalf("[FATAL] Dry Run Failed: Could not open test log file %s: %v", testLogPath, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0

	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		entry, parseErr := parseLogLine(line)
		if parseErr != nil {
			logOutput(LevelDebug, "DRYRUN_WARN", "[Line %d]: Failed to parse log line: %v", lineNumber, parseErr)
			continue
		}

		CheckChains(entry)

		// Update LastRequestTime after chain checks to preserve the time of the *previous* hit for next log entry.
		GetOrCreateActivity(entry.IP).LastRequestTime = entry.Timestamp
	}

	if err := scanner.Err(); err != nil {
		logOutput(LevelError, "DRYRUN_ERROR", "Reading test log file: %v", err)
	}

	logOutput(LevelInfo, "DRYRUN", "COMPLETE: Review output for 'DRY RUN' messages.")
	logOutput(LevelInfo, "INFO", "Total distinct IPs processed: %d", len(dryRunActivityStore))
}

// TailLogWithRotation tails a log file indefinitely, supporting rotation via inode checks.
func TailLogWithRotation() {
	if dryRun {
		return
	}

	logOutput(LevelInfo, "TAIL", "Starting live log tailing on %s with rotation support...", logFilePath)

	for {
		// 1. Open the file and seek to the end.
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

		// Get initial file metadata (Dev and Inode) to detect rotation.
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

		reader := bufio.NewReader(file) // Create buffered reader for efficiency
		logOutput(LevelInfo, "TAIL", "Now tailing (Dev: %d, Inode: %d)", initialDev, initialIno)

		// 2. Inner loop for active tailing.
		for {
			line, err := reader.ReadString('\n')
			if err == nil {
				line = strings.TrimSpace(line)
				if line != "" {
					if entry, parseErr := parseLogLine(line); parseErr == nil {
						CheckChains(entry) // Process the log entry
						
						// Update LastRequestTime after chain checks to preserve the time of the *previous* hit for next log entry.
						GetOrCreateActivity(entry.IP).LastRequestTime = entry.Timestamp
					}
				}
			} else if err.Error() == "EOF" {
				// Check 1: Did the size shrink? (Truncation/rotation)
				currentStat, err := os.Stat(logFilePath)
				if err == nil && currentStat.Size() < initialStat.Size() {
					logOutput(LevelDebug, "TAIL", "Detected log file size reduction (truncation/rotation). Reopening file.")
					file.Close()
					break
				}

				// Check 2: Did the inode change? (Standard rotation)
				currentFileInfo, err := os.Stat(logFilePath)
				if err != nil {
					logOutput(LevelError, "TAIL_ERROR", "Failed to stat log path during EOF check: %v. Reopening in 1s.", err)
					time.Sleep(1 * time.Second)
					file.Close()
					break
				}
				currentSysStat := currentFileInfo.Sys().(*syscall.Stat_t)

				if currentSysStat.Dev != initialDev || currentSysStat.Ino != initialIno {
					logOutput(LevelInfo, "TAIL", "Detected log file rotation (Inode changed from %d to %d). Reopening file.", initialIno, currentSysStat.Ino)
					file.Close()
					break
				}

				// No rotation detected, wait for more data.
				time.Sleep(200 * time.Millisecond)
			} else {
				// Handle other I/O errors.
				logOutput(LevelError, "TAIL_ERROR", "Reading log file: %v. Reopening in 1s.", err)
				time.Sleep(1 * time.Second)
				file.Close()
				break
			}
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
		DryRunLogProcessor()
	} else {
		logOutput(LevelInfo, "INFO", "Running in Production Mode with per-attempt HAProxy Fail-Safe. Log level set to %s.", strings.ToUpper(logLevelStr))

		// GRACEFUL SHUTDOWN SETUP
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

		if fileInfo, err := os.Stat(yamlFilePath); err == nil {
			lastModTime = fileInfo.ModTime()
		}

		// Start essential goroutines
		go ChainWatcher()
		go CleanUpIdleActivity()
		go TailLogWithRotation()

		// Wait here for the interrupt signal.
		<-stop
		logOutput(LevelCritical, "SHUTDOWN", "Interrupt signal received. Shutting down gracefully...")
		logOutput(LevelCritical, "SHUTDOWN", "Exiting.")
	}
}
