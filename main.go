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
	// Log & HAProxy paths
	logFilePath       string
	haproxySocketPath string
	blockedMapPath    string

	// Configuration file options
	yamlFilePath       string
	pollingIntervalStr string

	// Memory management options
	cleanupIntervalStr string
	idleTimeoutStr     string

	// DRY RUN OPTIONS
	dryRun      bool
	testLogPath string

	// Parsed durations
	pollingInterval time.Duration
	cleanupInterval time.Duration
	idleTimeout     time.Duration
)

// --- YAML DATA STRUCTURES ---

// StepDefYAML defines one action in a behavioral chain from the YAML file
type StepDefYAML struct {
	Order           int               `yaml:"order"`
	FieldMatches    map[string]string `yaml:"field_matches"`
	MaxDelaySeconds string            `yaml:"max_delay"` // e.g., "5s", "10m"
}

// BehavioralChainYAML is the structure for a chain in the YAML file
type BehavioralChainYAML struct {
	Name          string        `yaml:"name"`
	Steps         []StepDefYAML `yaml:"steps"`
	Action        string        `yaml:"action"`
	BlockDuration string        `yaml:"block_duration"`
}

// ChainConfig is the root structure for the entire YAML file, including versioning
type ChainConfig struct {
	Version string                `yaml:"version"`
	Chains  []BehavioralChainYAML `yaml:"chains"`
}

// --- RUNTIME DATA STRUCTURES ---

// LogEntry captures essential fields for analysis
type LogEntry struct {
	Timestamp  time.Time
	IP         string
	Path       string
	Method     string
	UserAgent  string
	Referrer   string
	StatusCode int
}

// StepDef (Runtime) includes pre-compiled regex and parsed duration
type StepDef struct {
	Order            int
	FieldMatches     map[string]string
	MaxDelayDuration time.Duration // Parsed duration
	compiledRegexes  map[string]*regexp.Regexp
}

// BehavioralChain (Runtime) includes parsed block duration and runtime steps
type BehavioralChain struct {
	Name          string
	Steps         []StepDef
	Action        string
	BlockDuration time.Duration
}

// StepState tracks an IP's progress through a single chain
type StepState struct {
	CurrentStep   int
	LastMatchTime time.Time
}

// BotActivity tracks state for a single IP address
type BotActivity struct {
	LastRequestTime time.Time
	ChainProgress   map[string]StepState
}

// --- GLOBAL STATE ---
var (
	// Production state store, protected by ActivityMutex
	ActivityStore = make(map[string]*BotActivity)
	ActivityMutex sync.Mutex

	// Dynamic runtime chains, protected by a read/write mutex
	Chains      []BehavioralChain
	ChainMutex  sync.RWMutex
	lastModTime time.Time

	// Dry Run state storage (separate for isolation)
	dryRunActivityStore = make(map[string]*BotActivity)
	dryRunActivityMutex sync.Mutex
)

// --- 🧩 CLI FLAG INITIALIZATION AND PARSING ---

func init() {
	// Log & HAProxy paths
	flag.StringVar(&logFilePath, "log-path", "/var/log/http/access.log", "Path to the live access log file to tail (ignored in dry-run).")
	flag.StringVar(&haproxySocketPath, "socket-path", "/var/run/haproxy.sock", "Path to the HAProxy Runtime API Unix socket (ignored in dry-run).")
	flag.StringVar(&blockedMapPath, "map-path", "/etc/haproxy/maps/blocked_ips.map", "Path to the HAProxy map file used for dynamic IP blocking (ignored in dry-run).")

	// Configuration file options
	flag.StringVar(&yamlFilePath, "yaml-path", "chains.yaml", "Path to the YAML configuration file defining behavioral chains.")
	flag.StringVar(&pollingIntervalStr, "poll-interval", "5s", "Interval (e.g., '10s', '1m') to check the YAML file for changes (ignored in dry-run).")

	// Memory management options (Critical for leak prevention)
	flag.StringVar(&cleanupIntervalStr, "cleanup-interval", "1m", "Interval (e.g., '5m') to run the routine that cleans up idle IP state.")
	flag.StringVar(&idleTimeoutStr, "idle-timeout", "30m", "Duration (e.g., '45m') an IP must be inactive before its state is purged from memory.")

	// DRY RUN OPTIONS
	flag.BoolVar(&dryRun, "dry-run", false, "If true, runs in test mode: skips HAProxy/live logging, ignores cleanup/polling, and uses --test-log.")
	flag.StringVar(&testLogPath, "test-log", "test_access.log", "Path to a static file containing log lines for dry-run testing.")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "A behavioral bot detection tool that monitors logs and blocks malicious IPs via the HAProxy Runtime API.\n\n")
		fmt.Fprintf(os.Stderr, "Configuration Options (--option value):\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nMemory and CPU are optimized by pre-compiling regexes and using the cleanup routine.\n")
	}
}

// parseDurations converts string flags to time.Duration
func parseDurations() error {
	var err error

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
		log.Printf("[DRYRUN] Would block IP %s for %v (Chain complete).", ip, duration)
		return nil
	}

	haproxyDuration := fmt.Sprintf("%.0fs", duration.Seconds())
	command := fmt.Sprintf("set map %s %s true timeout %s\n", blockedMapPath, ip, haproxyDuration)

	conn, err := net.Dial("unix", haproxySocketPath)
	if err != nil {
		// FAIL-SAFE IMPLEMENTATION: If the connection fails (e.g., socket missing/HAProxy down),
		// log the error, then downgrade the block action to a log action.
		log.Printf("[ERROR] Failed to connect to HAProxy socket %s during block attempt: %v", haproxySocketPath, err)
		log.Printf("[FAILSAFE] Block for IP %s downgraded to LOG action.", ip)
		return nil
	}
	defer conn.Close()

	if _, err = conn.Write([]byte(command)); err != nil {
		return fmt.Errorf("failed to send command to HAProxy: %w", err)
	}

	// NEW: Read Response Confirmation
	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')

	// An EOF error can sometimes indicate a successful, immediate socket close after HAProxy accepts the command.
	// We only treat it as an error if it's not EOF.
	if err != nil && err.Error() != "EOF" {
		return fmt.Errorf("HAProxy response read error for IP %s: %w", ip, err)
	}

	trimmedResponse := strings.TrimSpace(response)
	// Check for explicit error messages (HAProxy often prints '500' or similar prefix on failure)
	if strings.HasPrefix(trimmedResponse, "500") || strings.Contains(trimmedResponse, "error") {
		return fmt.Errorf("HAProxy execution error for IP %s: %s", ip, trimmedResponse)
	}
	// END NEW

	log.Printf("[HAPROXY] IP %s blocked for %v (via map: %s)", ip, duration, blockedMapPath)
	return nil
}

// --- MEMORY LEAK PREVENTION ROUTINE (Critical) ---

func CleanUpIdleActivity() {
	if dryRun {
		return
	}

	log.Printf("[CLEANUP] Starting Cleanup routine. Purging state older than %v every %v.", idleTimeout, cleanupInterval)
	for {
		time.Sleep(cleanupInterval)

		ActivityMutex.Lock()
		now := time.Now()
		deletedCount := 0

		for ip, activity := range ActivityStore {
			// Check if last request time exceeds the idle timeout
			if now.Sub(activity.LastRequestTime) > idleTimeout {
				delete(ActivityStore, ip)
				deletedCount++
			}
		}
		ActivityMutex.Unlock()

		if deletedCount > 0 {
			log.Printf("[CLEANUP] Complete: Purged %d idle IP states. Current active IPs: %d", deletedCount, len(ActivityStore))
		}
	}
}

// --- YAML LOADING & WATCHER LOGIC (CPU-optimized) ---

// loadChainsFromYAML reads, parses, and compiles regexes for the chains.
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
		// Parse block duration
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

			// Parse max delay
			if yamlStep.MaxDelaySeconds != "" {
				runtimeStep.MaxDelayDuration, err = time.ParseDuration(yamlStep.MaxDelaySeconds)
				if err != nil {
					return nil, fmt.Errorf("chain '%s', step %d: failed to parse max_delay '%s': %w", yamlChain.Name, yamlStep.Order, yamlStep.MaxDelaySeconds, err)
				}
			}

			// Compile regexes (Case-sensitive by default, which is the desired behavior)
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

func ChainWatcher() {
	if dryRun {
		return
	}
	log.Printf("[WATCH] Starting ChainWatcher, polling %s every %v", yamlFilePath, pollingInterval)
	for {
		time.Sleep(pollingInterval)

		fileInfo, err := os.Stat(yamlFilePath)
		if err != nil {
			log.Printf("[WATCH:ERROR] Failed to stat file %s: %v", yamlFilePath, err)
			continue
		}
		modTime := fileInfo.ModTime()

		if modTime.After(lastModTime) {
			log.Println("[WATCH] Detected change in chains.yaml. Attempting reload...")

			newChains, err := loadChainsFromYAML()
			if err != nil {
				log.Printf("[LOAD:ERROR] Failed to reload chains: %v. Retaining previous configuration.", err)
				continue
			}

			ChainMutex.Lock()
			Chains = newChains
			lastModTime = modTime
			ChainMutex.Unlock()
			log.Printf("[LOAD] Successfully reloaded and compiled %d behavioral chains.", len(newChains))
		}
	}
}

// --- CORE BEHAVIORAL ANALYSIS ---

// GetOrCreateActivity uses the appropriate state store based on the run mode.
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
		activity.LastRequestTime = time.Now()
		return activity
	}

	newActivity := &BotActivity{
		LastRequestTime: time.Now(),
		ChainProgress:   make(map[string]StepState),
	}
	store[ip] = newActivity
	return newActivity
}

func CheckChains(entry *LogEntry) {
	activity := GetOrCreateActivity(entry.IP)

	// Helper function to get a field value
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

		// Use the correct mutex based on run mode
		mutex := &ActivityMutex
		if dryRun {
			mutex = &dryRunActivityMutex
		}

		mutex.Lock()
		state, exists := activity.ChainProgress[chain.Name]
		if !exists {
			state = StepState{}
		}
		mutex.Unlock()

		nextStepIndex := state.CurrentStep
		if nextStepIndex >= len(chain.Steps) {
			nextStepIndex = 0
		}

		// 1. Time Check (CPU-efficient comparison)
		if state.CurrentStep > 0 && nextStepIndex < len(chain.Steps) {
			prevStep := chain.Steps[state.CurrentStep-1]
			delay := entry.Timestamp.Sub(state.LastMatchTime)

			if prevStep.MaxDelayDuration > 0 && delay > prevStep.MaxDelayDuration {
				state.CurrentStep = 0
				nextStepIndex = 0
			}
		}

		// 2. Generic Field Match Check
		if nextStepIndex < len(chain.Steps) {
			nextStep := chain.Steps[nextStepIndex]
			allFieldsMatch := true

			for fieldName := range nextStep.FieldMatches {
				regex := nextStep.compiledRegexes[fieldName]
				fieldValue := ""
				var err error

				switch fieldName {
				case "Referrer":
					// STATIC Referrer Match (Matches log entry's Referrer field against regex)
					fieldValue = entry.Referrer
				case "ReferrerPrevPath":
					// DYNAMIC Referrer Match (Extract and match against the path component)
					if entry.Referrer != "" {
						u, parseErr := url.Parse(entry.Referrer)
						if parseErr == nil && u.Path != "" {
							// Successfully parsed; use only the path for matching
							fieldValue = u.Path
						} else {
							// If parsing fails or path is empty, log warning and use full string
							// (which will likely fail the path regex, acting as a non-match)
							if !dryRun {
								log.Printf("[WARN] Failed to parse URL path from referrer: %s (Error: %v)", entry.Referrer, parseErr)
							}
							fieldValue = entry.Referrer
						}
					} else {
						// Referrer is empty, which means it cannot match a path regex.
						allFieldsMatch = false
						break
					}
				case "StatusCode":
					fieldValue = strconv.Itoa(entry.StatusCode)
				default:
					fieldValue, err = getMatchValue(fieldName, entry)
				}

				if err != nil {
					log.Printf("[ERROR] Internal error in getMatchValue for field %s: %v", fieldName, err)
					allFieldsMatch = false
					break
				}

				// Case-Sensitive Match performed by default Go regex behavior (Optimized via pre-compilation)
				if !regex.MatchString(fieldValue) {
					allFieldsMatch = false
					break
				}
			}

			if allFieldsMatch {
				// Match successful. Advance progress.
				state.CurrentStep++
				state.LastMatchTime = entry.Timestamp

				// 3. Check for Chain Completion
				if state.CurrentStep == len(chain.Steps) {
					log.Printf("[ALERT] BEHAVIORAL MATCH! Chain: %s completed by IP %s.", chain.Name, entry.IP)

					// Execute action based on the chain's runtime Action field
					if chain.Action == "block" {
						// BlockIPForDuration now handles the fail-safe internally
						if err := BlockIPForDuration(entry.IP, chain.BlockDuration); err != nil {
							// This error block catches write/response errors, not socket connection errors
							log.Printf("[ERROR] During HAProxy block command execution: %v", err)
						}
					} else if chain.Action == "log" {
						log.Printf("[LOG] ACTION: Chain '%s' completed. IP %s recorded.", chain.Name, entry.IP)
					}

					state.CurrentStep = 0
				}
			}
		}

		// Update the state map
		mutex.Lock()
		activity.ChainProgress[chain.Name] = state
		mutex.Unlock()
	}
}

// --- LOG PARSING & TAILING (MODIFIED TO HANDLE ROTATION) ---

// parseLogLine is a mock parser.
func parseLogLine(line string) (*LogEntry, error) {
	// Assumed format: HOSTNAME IP - - [TIME] "METHOD PATH HTTP/1.1" STATUS SIZE "REFERRER" "USERAGENT"
	parts := strings.Split(line, "\"")
	if len(parts) < 5 {
		return nil, fmt.Errorf("malformed log line")
	}

	ipPart := strings.Fields(parts[0])
	requestPart := strings.Fields(parts[1])
	statusSizePart := strings.Fields(parts[2])

	// CRITICAL FIX: Ensure at least two fields are present before the timestamp (Hostname and IP)
	if len(ipPart) < 2 || len(requestPart) < 2 || len(statusSizePart) < 1 {
		return nil, fmt.Errorf("malformed essential fields (missing IP, Hostname, or Request)")
	}

	// CORRECTED: Extract the second field (index 1) which is the client IP address
	ip := ipPart[1]
	method := requestPart[0]
	path := requestPart[1]
	referrer := parts[3]
	userAgent := parts[5]

	statusCode, err := strconv.Atoi(statusSizePart[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse status code: %w", err)
	}

	return &LogEntry{
		Timestamp:  time.Now(),
		IP:         ip,
		Path:       path,
		Method:     method,
		StatusCode: statusCode,
		Referrer:   strings.TrimSpace(referrer),
		UserAgent:  strings.TrimSpace(userAgent),
	}, nil
}

// DryRunLogProcessor uses bufio.Scanner (efficient for reading static files)
func DryRunLogProcessor() {
	log.Printf("[DRYRUN] MODE: Reading test logs from %s...", testLogPath)

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
			log.Printf("[DRYRUN:WARN] [Line %d]: Failed to parse log line: %v", lineNumber, parseErr)
			continue
		}

		CheckChains(entry)
	}

	if err := scanner.Err(); err != nil {
		log.Printf("[DRYRUN:ERROR] Reading test log file: %v", err)
	}

	log.Println("[DRYRUN] COMPLETE: Review output for 'DRY RUN' messages.")
	log.Printf("[INFO] Total distinct IPs processed: %d", len(dryRunActivityStore))
}

// TailLogWithRotation handles file rotation by checking file inode changes.
func TailLogWithRotation() {
	if dryRun {
		return
	}

	log.Printf("[TAIL] Starting live log tailing on %s with rotation support...", logFilePath)

	// The main loop that manages the file handle
	for {
		// 1. Open the file and seek to the end
		file, err := os.OpenFile(logFilePath, os.O_RDONLY, 0644)
		if err != nil {
			log.Printf("[TAIL:ERROR] Failed to open log file %s: %v. Retrying in 5s.", logFilePath, err)
			time.Sleep(5 * time.Second)
			continue
		}
		// NOTE: file.Close() is called by defer outside the inner loop,
		// but since the outer loop is infinite, we explicitly close on a failed seek.

		_, err = file.Seek(0, 2)
		if err != nil {
			log.Printf("[TAIL:ERROR] Failed to seek to end of log file: %v. Closing and retrying.", err)
			file.Close() // Close the failed file handle
			time.Sleep(1 * time.Second)
			continue
		}

		// Get the initial stat information to check for rotation
		initialStat, err := file.Stat()
		if err != nil {
			log.Printf("[TAIL:ERROR] Failed to stat open file: %v. Closing and retrying.", err)
			file.Close()
			time.Sleep(1 * time.Second)
			continue
		}
		initialSysStat := initialStat.Sys().(*syscall.Stat_t)
		initialDev := initialSysStat.Dev
		initialIno := initialSysStat.Ino

		reader := bufio.NewReader(file)
		log.Printf("[TAIL] Now tailing (Dev: %d, Inode: %d)", initialDev, initialIno)

		// 2. Inner loop for active tailing
		for {
			line, err := reader.ReadString('\n')
			if err == nil {
				// Log line read successfully
				line = strings.TrimSpace(line)
				if line != "" {
					if entry, parseErr := parseLogLine(line); parseErr == nil {
						CheckChains(entry)
					}
				}
			} else if err.Error() == "EOF" {
				// We reached the end of the file. Check for rotation before sleeping.

				// Check 1: Did the size shrink? (Rare, but indicates truncation/rotation)
				currentStat, err := os.Stat(logFilePath)
				if err == nil && currentStat.Size() < initialStat.Size() {
					log.Println("[TAIL] Detected log file size reduction (truncation/rotation). Reopening file.")
					file.Close()
					break // Break inner loop to trigger outer loop and re-open
				}

				// Check 2: Did the inode change? (Standard rotation)
				currentFileInfo, err := os.Stat(logFilePath)
				if err != nil {
					// File might be missing or permission error. Break to re-attempt opening.
					log.Printf("[TAIL:ERROR] Failed to stat log path during EOF check: %v. Reopening in 1s.", err)
					time.Sleep(1 * time.Second)
					file.Close()
					break
				}
				currentSysStat := currentFileInfo.Sys().(*syscall.Stat_t)

				if currentSysStat.Dev != initialDev || currentSysStat.Ino != initialIno {
					log.Printf("[TAIL] Detected log file rotation (Inode changed from %d to %d). Reopening file.", initialIno, currentSysStat.Ino)
					file.Close()
					break // Break inner loop to trigger outer loop and re-open
				}

				// No rotation detected, wait for more data
				time.Sleep(200 * time.Millisecond)
			} else {
				// Some other error (e.g., I/O error). Log and break to re-open.
				log.Printf("[TAIL:ERROR] Reading log file: %v. Reopening in 1s.", err)
				time.Sleep(1 * time.Second)
				file.Close()
				break
			}
		}

		// If we reach here (via 'break'), the file handle is closed (explicitly or via defer in future loop)
		// and the outer loop will execute the next iteration, opening the new file.
		// Sleep briefly to avoid a hot loop if the file disappears.
		time.Sleep(100 * time.Millisecond)
	}
}

// --- MAIN FUNCTION ---

func main() {
	// 1. Parse CLI flags
	flag.Parse()

	// 2. Parse durations based on run mode
	if err := parseDurations(); err != nil {
		log.Fatalf("[FATAL] Configuration Error: %v", err)
	}

	// 3. Initial configuration load (Always load the YAML as is)
	var err error
	Chains, err = loadChainsFromYAML()
	if err != nil {
		log.Fatalf("[FATAL] Initial chain load failed: %v", err)
	}
	log.Printf("[LOAD] Initial configuration loaded. Loaded %d behavioral chains.", len(Chains))

	if dryRun {
		// DRY RUN PATH
		DryRunLogProcessor()
	} else {
		// PRODUCTION PATH
		log.Println("[INFO] Running in Production Mode with per-attempt HAProxy Fail-Safe.")

		// NEW: GRACEFUL SHUTDOWN SETUP
		// Set up a channel to listen for interrupt/termination signals (Ctrl+C, kill)
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		// END NEW

		if fileInfo, err := os.Stat(yamlFilePath); err == nil {
			lastModTime = fileInfo.ModTime()
		}

		// Start essential goroutines
		go ChainWatcher()
		go CleanUpIdleActivity()

		// Start the log processing goroutine
		go TailLogWithRotation()

		// Wait here for the interrupt signal to initiate graceful shutdown
		<-stop
		log.Println("[SHUTDOWN] Interrupt signal received. Shutting down gracefully...")
		log.Println("[SHUTDOWN] Exiting.")
	}
}
