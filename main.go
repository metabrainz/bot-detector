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

	dryRun      bool
	testLogPath string

	pollingInterval time.Duration
	cleanupInterval time.Duration
	idleTimeout     time.Duration
)

// --- YAML DATA STRUCTURES ---

type StepDefYAML struct {
	Order           int               `yaml:"order"`
	FieldMatches    map[string]string `yaml:"field_matches"`
	MaxDelaySeconds string            `yaml:"max_delay"`
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
	LastRequestTime time.Time
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
		// FAIL-SAFE: If connection fails, log error and return nil (downgrade action)
		log.Printf("[ERROR] Failed to connect to HAProxy socket %s during block attempt for IP %s: %v", haproxySocketPath, ip, err)
		log.Printf("[FAILSAFE] Block for IP %s downgraded to LOG action.", ip)
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
		return fmt.Errorf("HAProxy response read error for IP %s: %w", ip, err)
	}

	trimmedResponse := strings.TrimSpace(response)

	// Display the specific HAProxy error message (e.g., 500 or error keyword).
	if strings.HasPrefix(trimmedResponse, "500") || strings.Contains(trimmedResponse, "error") {
		log.Printf("[HAPROXY:ERROR] HAProxy execution failed for IP %s. Response: %s", ip, trimmedResponse)
		return fmt.Errorf("HAProxy execution error for IP %s: %s", ip, trimmedResponse)
	}

	log.Printf("[HAPROXY] IP %s blocked for %v (via map: %s)", ip, duration, blockedMapPath)
	return nil
}

// --- MEMORY LEAK PREVENTION ROUTINE ---

// CleanUpIdleActivity periodically purges state for IPs inactive longer than idleTimeout.
func CleanUpIdleActivity() {
	if dryRun {
		return
	}

	log.Printf("[CLEANUP] Starting Cleanup routine. Purging state older than %v every %v.", idleTimeout, cleanupInterval)
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
			log.Printf("[CLEANUP] Complete: Purged %d idle IP states. Current active IPs: %d", deletedCount, len(ActivityStore))
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

		// 1. Time Check: Reset progress if delay exceeds the Max Delay Duration.
		if state.CurrentStep > 0 && nextStepIndex < len(chain.Steps) {
			prevStep := chain.Steps[state.CurrentStep-1]
			delay := entry.Timestamp.Sub(state.LastMatchTime)

			if prevStep.MaxDelayDuration > 0 && delay > prevStep.MaxDelayDuration {
				state.CurrentStep = 0 // Reset chain progress
				nextStepIndex = 0
			}
		}

		// 2. Field Match Check
		if nextStepIndex < len(chain.Steps) {
			nextStep := chain.Steps[nextStepIndex]
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
							fieldValue = u.Path
						} else {
							if !dryRun {
								log.Printf("[WARN] Failed to parse URL path from referrer: %s (Error: %v)", entry.Referrer, parseErr)
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
					log.Printf("[ERROR] Internal error in getMatchValue for field %s: %v", fieldName, err)
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
				state.CurrentStep++
				state.LastMatchTime = entry.Timestamp

				// 3. Check for Chain Completion
				if state.CurrentStep == len(chain.Steps) {
					log.Printf("[ALERT] BEHAVIORAL MATCH! Chain: %s completed by IP %s.", chain.Name, entry.IP)

					if chain.Action == "block" {
						if err := BlockIPForDuration(entry.IP, chain.BlockDuration); err != nil {
							// Error is handled/logged in BlockIPForDuration.
						}
					} else if chain.Action == "log" {
						log.Printf("[LOG] ACTION: Chain '%s' completed. IP %s recorded.", chain.Name, entry.IP)
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
		return nil, fmt.Errorf("malformed log line")
	}

	ipPart := strings.Fields(parts[0])
	requestPart := strings.Fields(parts[1])
	statusSizePart := strings.Fields(parts[2])

	if len(ipPart) < 4 || len(requestPart) < 2 || len(statusSizePart) < 1 {
		return nil, fmt.Errorf("malformed essential fields")
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

	// Parse the actual timestamp from the log line (e.g., Apache combined log format).
	timeStr := strings.Trim(ipPart[3], "[]")

	// Use Go's reference time for layout: "02/Jan/2006:15:04:05 -0700".
	t, parseErr := time.Parse("02/Jan/2006:15:04:05 -0700", timeStr)
	if parseErr != nil {
		log.Printf("[PARSE:WARN] Failed to parse log time '%s'. Using current time: %v", timeStr, parseErr)
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

// TailLogWithRotation tails a log file indefinitely, supporting rotation via inode checks.
func TailLogWithRotation() {
	if dryRun {
		return
	}

	log.Printf("[TAIL] Starting live log tailing on %s with rotation support...", logFilePath)

	for {
		// 1. Open the file and seek to the end.
		file, err := os.OpenFile(logFilePath, os.O_RDONLY, 0644)
		if err != nil {
			log.Printf("[TAIL:ERROR] Failed to open log file %s: %v. Retrying in 5s.", logFilePath, err)
			time.Sleep(5 * time.Second)
			continue
		}

		_, err = file.Seek(0, 2)
		if err != nil {
			log.Printf("[TAIL:ERROR] Failed to seek to end of log file: %v. Closing and retrying.", err)
			file.Close()
			time.Sleep(1 * time.Second)
			continue
		}

		// Get initial file metadata (Dev and Inode) to detect rotation.
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

		// 2. Inner loop for active tailing.
		for {
			line, err := reader.ReadString('\n')
			if err == nil {
				line = strings.TrimSpace(line)
				if line != "" {
					if entry, parseErr := parseLogLine(line); parseErr == nil {
						CheckChains(entry)
					}
				}
			} else if err.Error() == "EOF" {
				// Check 1: Did the size shrink? (Truncation/rotation)
				currentStat, err := os.Stat(logFilePath)
				if err == nil && currentStat.Size() < initialStat.Size() {
					log.Println("[TAIL] Detected log file size reduction (truncation/rotation). Reopening file.")
					file.Close()
					break
				}

				// Check 2: Did the inode change? (Standard rotation)
				currentFileInfo, err := os.Stat(logFilePath)
				if err != nil {
					log.Printf("[TAIL:ERROR] Failed to stat log path during EOF check: %v. Reopening in 1s.", err)
					time.Sleep(1 * time.Second)
					file.Close()
					break
				}
				currentSysStat := currentFileInfo.Sys().(*syscall.Stat_t)

				if currentSysStat.Dev != initialDev || currentSysStat.Ino != initialIno {
					log.Printf("[TAIL] Detected log file rotation (Inode changed from %d to %d). Reopening file.", initialIno, currentSysStat.Ino)
					file.Close()
					break
				}

				// No rotation detected, wait for more data.
				time.Sleep(200 * time.Millisecond)
			} else {
				// Handle other I/O errors.
				log.Printf("[TAIL:ERROR] Reading log file: %v. Reopening in 1s.", err)
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
	log.Printf("[LOAD] Initial configuration loaded. Loaded %d behavioral chains.", len(Chains))

	if dryRun {
		DryRunLogProcessor()
	} else {
		log.Println("[INFO] Running in Production Mode with per-attempt HAProxy Fail-Safe.")

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
		log.Println("[SHUTDOWN] Interrupt signal received. Shutting down gracefully...")
		log.Println("[SHUTDOWN] Exiting.")
	}
}
