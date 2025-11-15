//go:build !test

package main

import (
	"bot-detector/internal/blocker"
	"bot-detector/internal/logging"
	metrics "bot-detector/internal/metrics"
	"bot-detector/internal/persistence"
	"bot-detector/internal/server"
	"bot-detector/internal/store"
	"bot-detector/internal/utils"
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort" // Added for sorting step metrics
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// signalMap maps common signal names to their syscall values.
var signalMap = map[string]os.Signal{
	"HUP":  syscall.SIGHUP,
	"USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2,
}

// main is the application entry point.
func main() {
	cliFlags := RegisterCLIFlags(flag.CommandLine)
	// Parse CLI flags
	flag.Parse()

	// Handle --check flag first. If present, validate config and exit.
	if *cliFlags.Check {
		if *cliFlags.ConfigPath == "" {
			log.Println("[FATAL] --config flag is required for --check")
			os.Exit(1)
		}
		absConfigPath, err := filepath.Abs(*cliFlags.ConfigPath)
		if err != nil {
			log.Printf("[FATAL] Could not determine absolute path for config file: %v\n", err)
			os.Exit(1)
		}
		opts := LoadConfigOptions{
			ConfigPath: absConfigPath,
		}
		_, err = LoadConfigFromYAML(opts)
		if err != nil {
			log.Printf("[FATAL] Configuration check failed: %v\n", err)
			os.Exit(1)
		}
		log.Println("[SUCCESS] Configuration is valid.")
		os.Exit(0)
	}

	// Handle the version flag first. If present, print version and exit.
	if *cliFlags.ShowVersion {
		fmt.Printf("bot-detector version %s\n", AppVersion)
		os.Exit(0)
	}

	// Validate that required flags are provided.
	// --log-path is required for live mode, but optional for dry-run (stdin). --config is always required.
	if *cliFlags.ConfigPath == "" || (*cliFlags.LogPath == "" && !*cliFlags.DryRun) {
		flag.Usage()
		os.Exit(1)
	}

	// Resolve the config path to an absolute path to make file matcher paths relative to it.
	absConfigPath, err := filepath.Abs(*cliFlags.ConfigPath)
	if err != nil {
		log.Fatalf("[FATAL] Could not determine absolute path for config file: %v", err)
	}
	// Load initial configuration from YAML.
	opts := LoadConfigOptions{
		ConfigPath: absConfigPath,
	}
	loadedCfg, err := LoadConfigFromYAML(opts)
	if err != nil {
		log.Fatalf("[FATAL] Configuration Load Error: %v", err)
	}

	logging.SetLogLevel(loadedCfg.LogLevel)

	// Create the config struct from the loaded data.
	appConfig := &AppConfig{
		GoodActors:               loadedCfg.GoodActors,
		BlockTableNameFallback:   loadedCfg.BlockTableNameFallback,
		CleanupInterval:          loadedCfg.CleanupInterval,
		DefaultBlockDuration:     loadedCfg.DefaultBlockDuration,
		DurationToTableName:      loadedCfg.DurationToTableName,
		EOFPollingDelay:          loadedCfg.EOFPollingDelay,
		FileDependencies:         loadedCfg.FileDependencies,
		BlockerAddresses:         loadedCfg.BlockerAddresses,
		BlockerDialTimeout:       loadedCfg.BlockerDialTimeout,
		BlockerMaxRetries:        loadedCfg.BlockerMaxRetries,
		BlockerRetryDelay:        loadedCfg.BlockerRetryDelay,
		BlockerCommandQueueSize:  loadedCfg.BlockerCommandQueueSize,
		BlockerCommandsPerSecond: loadedCfg.BlockerCommandsPerSecond,
		IdleTimeout:              loadedCfg.IdleTimeout,
		LastModTime:              time.Now(),
		MaxTimeSinceLastHit:      loadedCfg.MaxTimeSinceLastHit,
		OutOfOrderTolerance:      loadedCfg.OutOfOrderTolerance,
		PollingInterval:          loadedCfg.PollingInterval,
		TimestampFormat:          loadedCfg.TimestampFormat,
		EnableMetrics:            loadedCfg.EnableMetrics,
		StatFunc:                 defaultStatFunc, // Initialize StatFunc to prevent nil pointer panic.
		FileOpener: func(name string) (fileHandle, error) {
			return os.Open(name)
		},
	}

	// Initialize the Processor instance.
	p := &Processor{
		ActivityMutex:        &sync.RWMutex{},
		TopActorsPerChain:    make(map[string]map[string]*store.ActorStats),
		ActivityStore:        make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:          &sync.RWMutex{},
		Metrics:              metrics.NewMetrics(),
		Chains:               loadedCfg.Chains,
		Config:               appConfig,
		LogRegex:             loadedCfg.LogFormatRegex,
		DryRun:               *cliFlags.DryRun,
		EnableMetrics:        loadedCfg.EnableMetrics,
		oooBufferFlushSignal: make(chan struct{}, 1), // Buffered channel of size 1
		signalCh:             make(chan os.Signal, 1),
		LogFunc:              logging.LogOutput,
		NowFunc:              time.Now, // Use the real time.Now in production.
		ConfigPath:           absConfigPath,
		LogPath:              *cliFlags.LogPath,
		ReloadOn:             *cliFlags.ReloadOn,
		TopN:                 *cliFlags.TopN,
		HTTPServer:           *cliFlags.HTTPServer,
		configReloaded:       false,

		// Initialize persistence fields
		persistenceEnabled: loadedCfg.Persistence.Enabled,
		stateDir:           loadedCfg.Persistence.StateDir,
		compactionInterval: loadedCfg.Persistence.CompactionInterval,
		activeBlocks:       make(map[string]persistence.ActiveBlockInfo),
	}
	p.startTime = p.NowFunc() // Record the start time.
	// TestSignals is intentionally left nil in production.
	// Set up the signalOooBufferFlush field to call the method.
	p.signalOooBufferFlush = p.doSignalOooBufferFlush
	initializeMetrics(p, loadedCfg)

	if p.persistenceEnabled {
		// -- STATE RESTORATION --
		p.LogFunc(logging.LevelInfo, "SETUP", "Persistence enabled. Loading state from '%s'...", p.stateDir)

		// 1. Load snapshot
		snapshot, err := persistence.LoadSnapshot(filepath.Join(p.stateDir, "state.snapshot"))
		if err != nil {
			p.LogFunc(logging.LevelError, "STATE_LOAD_FAIL", "Failed to load snapshot: %v", err)
			os.Exit(1)
		}
		p.activeBlocks = snapshot.ActiveBlocks

		// 2. Replay Journal
		journalPath := filepath.Join(p.stateDir, "events.log")
		journalFile, err := os.Open(journalPath)
		if err == nil {
			scanner := bufio.NewScanner(journalFile)
			for scanner.Scan() {
				var event persistence.AuditEvent
				if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
					p.LogFunc(logging.LevelWarning, "JOURNAL_PARSE_FAIL", "Failed to parse journal event: %v", err)
					continue
				}
				if event.Timestamp.After(snapshot.Timestamp) {
					switch event.Event {
					case persistence.EventTypeBlock:
						p.activeBlocks[event.IP] = persistence.ActiveBlockInfo{
							UnblockTime: event.Timestamp.Add(event.Duration),
							Reason:      event.Reason,
						}
					case persistence.EventTypeUnblock:
						delete(p.activeBlocks, event.IP)
					}
				}
			}
			journalFile.Close()
		} else if !os.IsNotExist(err) {
			p.LogFunc(logging.LevelWarning, "JOURNAL_OPEN_FAIL", "Could not open journal file for replay: %v", err)
		}

		// 3. State Push
		p.LogFunc(logging.LevelInfo, "STATE_RESTORE", "Restoring %d active blocks to backend...", len(p.activeBlocks))
		// Create a sorted list of table durations for best-fit matching
		type tableInfo struct {
			duration time.Duration
			name     string
		}
		var sortedTables []tableInfo
		for d, n := range p.Config.DurationToTableName {
			sortedTables = append(sortedTables, tableInfo{duration: d, name: n})
		}
		sort.Slice(sortedTables, func(i, j int) bool {
			return sortedTables[i].duration < sortedTables[j].duration
		})

		for ip, info := range p.activeBlocks {
			remainingDuration := time.Until(info.UnblockTime)
			if remainingDuration > 0 {
				bestFitDuration := p.Config.DefaultBlockDuration
				for _, t := range sortedTables {
					if remainingDuration <= t.duration {
						bestFitDuration = t.duration
						break
					}
				}
				p.Blocker.Block(utils.NewIPInfo(ip), bestFitDuration, info.Reason)
			}
		}

		// 4. Open journal for appending
		p.journalHandle, err = persistence.OpenJournalForAppend(journalPath)
		if err != nil {
			p.LogFunc(logging.LevelError, "JOURNAL_OPEN_FAIL", "Failed to open journal for writing: %v", err)
			os.Exit(1)
		}
	}

	haproxyBlocker := blocker.NewHAProxyBlocker(p, p.DryRun)
	rateLimitedBlocker := blocker.NewRateLimitedBlocker(p, p, haproxyBlocker, p.Config.BlockerCommandQueueSize, p.Config.BlockerCommandsPerSecond)

	p.Blocker = rateLimitedBlocker
	defer rateLimitedBlocker.Stop() // Ensure the rate limiter worker is stopped on exit.

	// Handle --list-blocked flag. If present, list blocked IPs and exit.
	if *cliFlags.ListBlocked {
		logging.LogOutput(logging.LevelInfo, "LIST_BLOCKED", "Retrieving currently blocked IPs from HAProxy...")
		blockedIPs, err := p.Blocker.ListBlocked()
		if err != nil {
			logging.LogOutput(logging.LevelError, "LIST_BLOCKED_FAIL", "Failed to retrieve blocked IPs: %v", err)
			os.Exit(1)
		}
		if len(blockedIPs) == 0 {
			logging.LogOutput(logging.LevelInfo, "LIST_BLOCKED", "No IPs currently blocked by HAProxy.")
		} else {
			logging.LogOutput(logging.LevelInfo, "LIST_BLOCKED", "Currently blocked IPs:")
			for _, ip := range blockedIPs {
				fmt.Println(ip)
			}
		}
		os.Exit(0)
	}

	p.CheckChainsFunc = func(entry *LogEntry) { CheckChains(p, entry) } // Assign the real method to the function field.

	// Assign the real implementation for ProcessLogLine, which no longer uses line numbers.
	p.ProcessLogLine = func(line string) { processLogLineInternal(p, line) }

	// Log the initial configuration summary just before starting the main loops.
	logConfigurationSummary(p)
	logChainDetails(p, p.Chains, "Loaded chains")

	start(p)
}

// --- MetricsProvider Interface Implementation ---

// GetListenAddr returns the HTTP listen address from the config.
func (p *Processor) GetListenAddr() string {
	return p.HTTPServer
}

// GetShutdownChannel returns the channel used for shutdown signals.
func (p *Processor) GetShutdownChannel() chan os.Signal {
	return p.signalCh
}

// Log is a wrapper around the processor's LogFunc to satisfy the interface.
func (p *Processor) Log(level logging.LogLevel, tag string, format string, v ...interface{}) {
	p.LogFunc(level, tag, format, v...)
}

// GenerateHTMLMetricsReport creates the full metrics report as an HTML-safe string.
func (p *Processor) GenerateHTMLMetricsReport() string {
	var report strings.Builder
	webLogFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		// Sanitize the formatted string before writing it to the HTML report.
		report.WriteString(utils.ForHTML(fmt.Sprintf(format, args...)) + "\n")
	}
	logMetricsSummary(p, time.Since(p.startTime), webLogFunc, "METRICS", "metric")
	return report.String()
}

// GenerateStepsMetricsReport creates a report of step execution counts as an HTML-safe string.
func (p *Processor) GenerateStepsMetricsReport() string {
	var report strings.Builder
	report.WriteString("--- Step Execution Counts ---\n")
	if p.Metrics.StepExecutionCounts == nil {
		report.WriteString("Step metrics are not enabled or initialized.\n")
		return report.String()
	}

	// Collect and sort step metrics for consistent output
	type StepMetric struct {
		Name  string
		Count int64
	}
	var stepMetrics []StepMetric
	p.Metrics.StepExecutionCounts.Range(func(key, value interface{}) bool {
		stepName, _ := key.(string)
		count, _ := value.(*atomic.Int64)
		stepMetrics = append(stepMetrics, StepMetric{Name: stepName, Count: count.Load()})
		return true
	})

	sort.Slice(stepMetrics, func(i, j int) bool {
		if stepMetrics[i].Count == stepMetrics[j].Count {
			return stepMetrics[i].Name < stepMetrics[j].Name
		}
		return stepMetrics[i].Count >= stepMetrics[j].Count
	})

	for _, sm := range stepMetrics {
		report.WriteString(fmt.Sprintf("%12d %s\n", sm.Count, utils.ForHTML(sm.Name)))
	}
	return report.String()
}

// --- ParserProvider Interface Implementation ---

func (p *Processor) GetTimestampFormat() string {
	return p.Config.TimestampFormat
}

func (p *Processor) GetLogRegex() *regexp.Regexp {
	p.ConfigMutex.RLock()
	defer p.ConfigMutex.RUnlock()
	return p.LogRegex
}

// --- StoreProvider Interface Implementation ---

func (p *Processor) GetCleanupInterval() time.Duration {
	return p.Config.CleanupInterval
}

func (p *Processor) GetIdleTimeout() time.Duration {
	return p.Config.IdleTimeout
}

func (p *Processor) GetMaxTimeSinceLastHit() time.Duration {
	return p.Config.MaxTimeSinceLastHit
}

func (p *Processor) GetTopN() int {
	return p.TopN
}

func (p *Processor) GetTopActorsPerChain() map[string]map[string]*store.ActorStats {
	return p.TopActorsPerChain
}

func (p *Processor) GetActivityStore() map[store.Actor]*store.ActorActivity {
	return p.ActivityStore
}

func (p *Processor) GetActivityMutex() *sync.RWMutex {
	return p.ActivityMutex
}

func (p *Processor) GetTestSignals() *store.TestSignals {
	if p.TestSignals == nil {
		return nil
	}
	// Convert main.TestSignals to store.TestSignals
	return &store.TestSignals{
		CleanupDoneSignal: p.TestSignals.CleanupDoneSignal,
	}
}

func (p *Processor) IncrementActorsCleaned() {
	p.Metrics.ActorsCleaned.Add(1)
}

// --- MetricsProvider Interface Implementation ---

func (p *Processor) IncrementBlockerCmdsQueued() {
	p.Metrics.BlockerCmdsQueued.Add(1)
}

func (p *Processor) IncrementBlockerCmdsDropped() {
	p.Metrics.BlockerCmdsDropped.Add(1)
}

func (p *Processor) IncrementBlockerCmdsExecuted() {
	p.Metrics.BlockerCmdsExecuted.Add(1)
}

// --- HAProxyProvider Interface Implementation ---

func (p *Processor) GetBlockerAddresses() []string {
	return p.Config.BlockerAddresses
}

func (p *Processor) GetDurationTables() map[time.Duration]string {
	return p.Config.DurationToTableName
}

func (p *Processor) GetBlockTableNameFallback() string {
	return p.Config.BlockTableNameFallback
}

func (p *Processor) GetBlockerMaxRetries() int {
	return p.Config.BlockerMaxRetries
}

func (p *Processor) GetBlockerRetryDelay() time.Duration {
	return p.Config.BlockerRetryDelay
}

func (p *Processor) GetBlockerDialTimeout() time.Duration {
	return p.Config.BlockerDialTimeout
}

func (p *Processor) IncrementBlockerRetries() {
	p.Metrics.BlockerRetries.Add(1)
}

func (p *Processor) IncrementCmdsPerBlocker(addr string) {
	if val, ok := p.Metrics.CmdsPerBlocker.Load(addr); ok {
		val.(*atomic.Int64).Add(1)
	}
}

// runCompaction creates a new snapshot and truncates the journal.
func runCompaction(p *Processor) {
	p.persistenceMutex.Lock()
	defer p.persistenceMutex.Unlock()

	// Filter out expired blocks before snapshotting
	now := p.NowFunc()
	activeBlocks := make(map[string]persistence.ActiveBlockInfo)
	for ip, info := range p.activeBlocks {
		if now.Before(info.UnblockTime) {
			activeBlocks[ip] = info
		}
	}
	p.activeBlocks = activeBlocks // Update in-memory map

	snapshot := &persistence.Snapshot{
		Timestamp:    now,
		ActiveBlocks: p.activeBlocks,
	}

	if err := persistence.WriteSnapshot(filepath.Join(p.stateDir, "state.snapshot"), snapshot); err != nil {
		p.LogFunc(logging.LevelError, "COMPACTION_FAIL", "Failed to write snapshot: %v", err)
		return
	}

	// Truncate and re-open journal
	journalPath := filepath.Join(p.stateDir, "events.log")
	if err := p.journalHandle.Close(); err != nil {
		p.LogFunc(logging.LevelError, "COMPACTION_FAIL", "Failed to close journal for truncation: %v", err)
	}

	// Re-open with truncation
	handle, err := os.OpenFile(journalPath, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		p.LogFunc(logging.LevelError, "COMPACTION_FAIL", "Failed to truncate and re-open journal: %v", err)
		// Attempt to re-open in append mode as a fallback
		handle, _ = persistence.OpenJournalForAppend(journalPath)
	}
	p.journalHandle = handle
	p.LogFunc(logging.LevelInfo, "COMPACTION", "State snapshot and journal compaction completed successfully.")
}

// start is the unexported function that contains the main application logic,
// which is called by the tests and the main function.
func start(p *Processor) {
	if p.DryRun {
		// DryRun mode: Process a static log file and exit when done.
		done := make(chan struct{})
		// Pass the Processor instance P
		go DryRunLogProcessor(p, done)

		// Wait for the processor to finish in dry-run mode
		<-done

	} else {
		// Live mode: Start background routines and the main log tailing loop.
		stopWatcher := make(chan struct{})
		defer close(stopWatcher) // Ensure watcher is stopped on main exit
		// Only start these background tasks if not in a test that bypasses them.
		// This allows tests to focus on specific components like the tailer without
		// interference from other goroutines like the config watcher.
		if !IsTesting() {
			// The ConfigWatcher is not started in test mode to prevent race conditions where
			// the test's config is overwritten by a reload from the default config file.
			reloadSignalCh := make(chan os.Signal, 1)
			signal.Notify(reloadSignalCh, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2)

			reloadFlag := strings.ToLower(p.ReloadOn)

			// Determine which reloading mechanisms to start based on the flag.
			switch reloadFlag {
			case "hup", "usr1", "usr2":
				// Flag is set to a specific signal. Watcher is disabled.
				p.LogFunc(logging.LevelInfo, "SETUP", "File watcher disabled. Reloading only on %s signal.", strings.ToUpper(reloadFlag))
				go SignalReloader(p, stopWatcher, reloadSignalCh)

			case "watcher":
				// Flag is 'watcher'. Signal reloading is disabled.
				p.LogFunc(logging.LevelInfo, "SETUP", "Signal-based reloading disabled. Watching config file for changes.")
				go ConfigWatcher(p, stopWatcher)

			case "":
				// Flag is absent. Both watcher and SIGHUP reloader are active.
				p.LogFunc(logging.LevelInfo, "SETUP", "File watcher enabled. Also listening on SIGHUP for forced reload.")
				go ConfigWatcher(p, stopWatcher)
				go SignalReloader(p, stopWatcher, reloadSignalCh) // This will default to HUP when p.ReloadOn is ""
			default:
				// An invalid value was provided. Log a fatal error and exit.
				log.Fatalf("[FATAL] Invalid value for --reload-on: '%s'. Must be one of 'watcher', 'hup', 'usr1', or 'usr2'.", p.ReloadOn)
			}

			if p.persistenceEnabled && p.compactionInterval > 0 {
				go func() {
					ticker := time.NewTicker(p.compactionInterval)
					defer ticker.Stop()
					p.LogFunc(logging.LevelInfo, "SETUP", "State compaction enabled, running every %v.", p.compactionInterval)
					for {
						select {
						case <-ticker.C:
							runCompaction(p)
						case <-stopWatcher:
							p.LogFunc(logging.LevelInfo, "SHUTDOWN", "Performing final state compaction...")
							runCompaction(p)
							return
						}
					}
				}()
			}

			if p.TopN > 0 {
				go cleanupTopActors(p, stopWatcher)
			}
			go store.CleanUpIdleActors(p, stopWatcher)
			go entryBufferWorker(p, stopWatcher)
			go server.Start(p)
		}
		// Listen for OS signals on the processor's channel
		signal.Notify(p.signalCh, syscall.SIGINT, syscall.SIGTERM)

		// LiveLogTailer is the blocking main loop
		LiveLogTailer(p, p.signalCh, nil) // Pass the processor's channel to the tailer
	}
}
