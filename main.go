package main

import (
	"bot-detector/internal/blocker"
	"bot-detector/internal/commandline"
	"bot-detector/internal/logging"
	"bot-detector/internal/metrics"
	"bot-detector/internal/persistence"
	"bot-detector/internal/server"
	"bot-detector/internal/store"
	"bot-detector/internal/types"
	"bot-detector/internal/utils"
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
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

// GetMarshalledConfig reads the raw configuration file from disk.
func (p *Processor) GetMarshalledConfig() ([]byte, time.Time, error) {
	return p.Config.YAMLContent, p.Config.LastModTime, nil
}

// Helper function to extract settings from the BuildInfo struct
func findSetting(info *debug.BuildInfo, key string) string {
	for _, setting := range info.Settings {
		if setting.Key == key {
			return setting.Value
		}
	}
	return "unknown"
}

func buildDetails() {
	// Use the runtime/debug package to get automatically embedded info
	info, ok := debug.ReadBuildInfo()
	if ok {
		fmt.Fprintf(os.Stderr, "\n--- BUILD DETAILS:\n")
		fmt.Fprintf(os.Stderr, "  Go Version:  %s\n", info.GoVersion)
		fmt.Fprintf(os.Stderr, "  Commit Hash: %s\n", findSetting(info, "vcs.revision"))
		fmt.Fprintf(os.Stderr, "  Build Time:  %s\n", findSetting(info, "vcs.time"))
		fmt.Fprintf(os.Stderr, "  Dirty Build: %s\n", findSetting(info, "vcs.modified"))
	}
}

func customPanic() {
	if r := recover(); r != nil {
		now := time.Now()
		fmt.Fprintf(os.Stderr, "--- START OF PANIC REPORT\n")
		fmt.Fprintf(os.Stderr, "Time: %s\n", now)
		fmt.Fprintf(os.Stderr, "Message: %v\n", r)

		fmt.Fprintf(os.Stderr, "\n--- STACK TRACE:\n")
		debug.PrintStack()
		buildDetails()
		fmt.Fprintf(os.Stderr, "\n--- END OF PANIC REPORT\n")

		os.Exit(1)
	}
}

// main is the application entry point.
func main() {
	defer customPanic()
	params, err := commandline.ParseParameters(os.Args)
	if err != nil {
		// A parsing error will have already printed usage information.
		// We exit with a non-zero code after the error is logged.
		log.Printf("[FATAL] %v", err)
		os.Exit(1)
	}

	if err := execute(params); err != nil {
		log.Printf("[FATAL] %v", err)
		os.Exit(1)
	}
}

// handleStartupFlags checks for command-line flags that prevent normal startup,
// such as --version or --check. It returns a special "exit" error to signal
// a clean exit, an error for failures, or nil to continue execution.
func handleStartupFlags(params *commandline.AppParameters) error {
	if params.ShowVersion {
		fmt.Printf("bot-detector version %s\n", AppVersion)
		return fmt.Errorf("exit") // Signal clean exit
	}

	if params.Check {
		opts := LoadConfigOptions{
			ConfigPath: params.ConfigPath,
		}
		var err error
		if _, err = LoadConfigFromYAML(opts); err != nil {
			return fmt.Errorf("configuration check failed: %v", err)
		}
		log.Println("[SUCCESS] Configuration is valid.")
		return fmt.Errorf("exit") // Signal clean exit
	}

	return nil // Continue execution
}

func initializeProcessor(params *commandline.AppParameters, appConfig *AppConfig, loadedCfg *LoadedConfig) *Processor {
	return &Processor{
		ActivityMutex:        &sync.RWMutex{},
		TopActorsPerChain:    make(map[string]map[string]*store.ActorStats),
		ActivityStore:        make(map[store.Actor]*store.ActorActivity),
		ConfigMutex:          &sync.RWMutex{},
		Metrics:              metrics.NewMetrics(),
		Chains:               loadedCfg.Chains,
		Config:               appConfig,
		LogRegex:             loadedCfg.LogFormatRegex,
		DryRun:               params.DryRun,
		ExitOnEOF:            params.ExitOnEOF,
		EnableMetrics:        loadedCfg.EnableMetrics,
		oooBufferFlushSignal: make(chan struct{}, 1), // Buffered channel of size 1
		signalCh:             make(chan os.Signal, 1),
		LogFunc:              logging.LogOutput,
		NowFunc:              time.Now, // Use the real time.Now in production.
		ConfigPath:           params.ConfigPath,
		LogPath:              params.LogPath,
		ReloadOn:             params.ReloadOn,
		TopN:                 params.TopN,
		HTTPServer:           params.HTTPServer,
		configReloaded:       false,

		// Initialize persistence fields
		persistenceEnabled: loadedCfg.Persistence.Enabled,
		compactionInterval: loadedCfg.Persistence.CompactionInterval,
		activeBlocks:       make(map[string]persistence.ActiveBlockInfo),
	}
}

func restorePersistenceState(p *Processor) error {
	// -- STATE RESTORATION --
	if err := os.MkdirAll(p.stateDir, 0750); err != nil {
		return fmt.Errorf("failed to create state directory '%s': %v", p.stateDir, err)
	}
	p.LogFunc(logging.LevelInfo, "SETUP", "Persistence enabled. Loading state from '%s'...", p.stateDir)

	// 1. Load snapshot
	snapshot, err := persistence.LoadSnapshot(filepath.Join(p.stateDir, "state.snapshot"))
	if err != nil {
		p.LogFunc(logging.LevelError, "STATE_LOAD_FAIL", "Failed to load snapshot: %v", err)
		return err
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
		if err := journalFile.Close(); err != nil {
			p.LogFunc(logging.LevelWarning, "JOURNAL_CLOSE_FAIL", "Failed to close journal file: %v", err)
		}
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
		// Before restoring, check if the IP is now a good actor.
		tempEntry := &LogEntry{IPInfo: utils.NewIPInfo(ip)}
		if isGood, reason := isGoodActor(p, tempEntry); isGood {
			p.LogFunc(logging.LevelInfo, "STATE_RESTORE_SKIP", "Skipping restore for %s (good_actor: %s)", ip, reason)
			continue // Don't restore blocks for good actors.
		}

		remainingDuration := time.Until(info.UnblockTime)
		if remainingDuration > 0 {
			bestFitDuration := p.Config.DefaultBlockDuration
			for _, t := range sortedTables {
				if remainingDuration <= t.duration {
					bestFitDuration = t.duration
					break
				}
			}
			if err := p.Blocker.Block(utils.NewIPInfo(ip), bestFitDuration, info.Reason); err != nil {
				p.LogFunc(logging.LevelError, "STATE_RESTORE_FAIL", "Failed to restore block for IP %s: %v", ip, err)
			}
		}
	}

	// 4. Open journal for appending
	p.journalHandle, err = persistence.OpenJournalForAppend(journalPath)
	if err != nil {
		p.LogFunc(logging.LevelError, "JOURNAL_OPEN_FAIL", "Failed to open journal for writing: %v", err)
		return err
	}
	return nil
}

// execute is the main application logic, decoupled from command-line parsing.
func execute(params *commandline.AppParameters) error {
	if err := handleStartupFlags(params); err != nil {
		// If handleStartupFlags returns an error, it means an early-exit flag
		// was handled successfully (e.g., --version) and the program should terminate
		// without an error code. A nil error from this function indicates that
		// normal execution should continue.
		if err.Error() == "exit" {
			return nil
		}
		return err
	}

	// Get the modification time of the config file to initialize LastModTime.
	fileInfo, err := os.Stat(params.ConfigPath)
	if err != nil {
		return fmt.Errorf("could not stat config file: %v", err)
	}
	// Load initial configuration from YAML.
	opts := LoadConfigOptions{
		ConfigPath: params.ConfigPath,
	}
	loadedCfg, err := LoadConfigFromYAML(opts)
	if err != nil {
		return fmt.Errorf("configuration Load Error: %v", err)
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
		LastModTime:              fileInfo.ModTime(),
		MaxTimeSinceLastHit:      loadedCfg.MaxTimeSinceLastHit,
		OutOfOrderTolerance:      loadedCfg.OutOfOrderTolerance,
		PollingInterval:          loadedCfg.PollingInterval,
		TimestampFormat:          loadedCfg.TimestampFormat,
		EnableMetrics:            loadedCfg.EnableMetrics,
		StatFunc:                 defaultStatFunc, // Initialize StatFunc to prevent nil pointer panic.
		FileOpener: func(name string) (fileHandle, error) {
			return os.Open(name)
		},
		YAMLContent: loadedCfg.YAMLContent,
	}

	p := initializeProcessor(params, appConfig, loadedCfg)

	// The --state-dir flag provides the path for persistence, but the config file
	// is the single source of truth for whether persistence is enabled.
	if params.StateDir != "" {
		p.stateDir = params.StateDir
	}

	// Final check: if persistence is enabled in YAML but no --state-dir is given, it's a fatal error.
	if p.persistenceEnabled && p.stateDir == "" {
		return fmt.Errorf("persistence is enabled in config, but no --state-dir was provided")
	}

	p.startTime = p.NowFunc() // Record the start time.
	// TestSignals is intentionally left nil in production.
	// Set up the signalOooBufferFlush field to call the method.
	p.signalOooBufferFlush = p.doSignalOooBufferFlush
	initializeMetrics(p, loadedCfg)

	haproxyBlocker := blocker.NewHAProxyBlocker(p, p.DryRun)
	rateLimitedBlocker := blocker.NewRateLimitedBlocker(p, p, haproxyBlocker, p.Config.BlockerCommandQueueSize, p.Config.BlockerCommandsPerSecond)
	p.Blocker = rateLimitedBlocker

	if p.persistenceEnabled {
		if err := restorePersistenceState(p); err != nil {
			return err
		}
	}

	haproxyBlocker = blocker.NewHAProxyBlocker(p, p.DryRun)
	rateLimitedBlocker = blocker.NewRateLimitedBlocker(p, p, haproxyBlocker, p.Config.BlockerCommandQueueSize, p.Config.BlockerCommandsPerSecond)

	p.Blocker = rateLimitedBlocker

	// Handle --dump-backends flag. If present, list blocked IPs and exit.
	if params.DumpBackends {
		// Only run the sync check if there are multiple backends to compare.
		if len(p.GetBlockerAddresses()) > 1 {
			logging.LogOutput(logging.LevelInfo, "SYNC_CHECK", "Checking HAProxy backend synchronization...")
			// Use a 5-second tolerance for expiration differences
			discrepancies, err := p.Blocker.CompareHAProxyBackends(5 * time.Second)
			if err != nil {
				logging.LogOutput(logging.LevelError, "SYNC_CHECK_FAIL", "Failed to compare HAProxy backends: %v", err)
				return err
			}

			if len(discrepancies) > 0 {
				logging.LogOutput(logging.LevelError, "SYNC_CHECK_FAIL", "HAProxy backends are out of sync. Aborting dump.")
				for _, d := range discrepancies {
					logging.LogOutput(logging.LevelError, "SYNC_CHECK_FAIL", "  - IP: %s, Table: %s, Reason: %s, Details: %v", d.IP, d.TableName, d.Reason, d.Details)
				}
				return fmt.Errorf("HAProxy backends are out of sync")
			}
			logging.LogOutput(logging.LevelInfo, "SYNC_CHECK", "HAProxy backends are in sync.")
		} else {
			logging.LogOutput(logging.LevelInfo, "SYNC_CHECK", "Skipping backend synchronization check (only one backend configured).")
		}

		logging.LogOutput(logging.LevelInfo, "DUMP_BACKENDS", "Retrieving currently blocked IPs from HAProxy...")
		// Add a small delay to allow the command queue to process restored blocks.
		time.Sleep(1 * time.Second)
		blockedIPs, err := p.Blocker.DumpBackends()
		if err != nil {
			logging.LogOutput(logging.LevelError, "DUMP_FAIL", "Failed to retrieve blocked IPs: %v", err)
			return err
		}
		if len(blockedIPs) == 0 {
			logging.LogOutput(logging.LevelInfo, "DUMP_BACKENDS", "No IPs currently blocked by HAProxy.")
		} else {
			logging.LogOutput(logging.LevelInfo, "DUMP_BACKENDS", "Currently blocked IPs:")
			for _, ip := range blockedIPs {
				fmt.Println(ip)
			}
		}
		return nil
	}

	p.CheckChainsFunc = func(entry *LogEntry) { CheckChains(p, entry) } // Assign the real method to the function field.

	// Assign the real implementation for ProcessLogLine, which no longer uses line numbers.
	p.ProcessLogLine = func(line string) { processLogLineInternal(p, line) }

	// Log the initial configuration summary just before starting the main loops.
	logConfigurationSummary(p)
	logChainDetails(p, p.Chains, "Loaded chains")

	start(p)

	// --- GRACEFUL SHUTDOWN SEQUENCE ---

	p.LogFunc(logging.LevelInfo, "SHUTDOWN", "Graceful shutdown initiated.")

	if p.Blocker != nil {
		p.Blocker.Shutdown()
	}

	if p.persistenceEnabled {
		// Wait for any pending writes to the journal.
		p.LogFunc(logging.LevelInfo, "PERSISTENCE", "Waiting for persistence operations to complete...")
		p.persistenceWg.Wait()

		// Perform final compaction if enabled.
		if p.compactionInterval > 0 {
			p.LogFunc(logging.LevelInfo, "SHUTDOWN", "Performing final state compaction...")
			runCompaction(p)
		}

		// Close the journal.
		p.LogFunc(logging.LevelInfo, "PERSISTENCE", "Closing journal file.")
		if p.journalHandle != nil {
			if err := p.journalHandle.Close(); err != nil {
				p.LogFunc(logging.LevelError, "PERSISTENCE", "Error closing journal file: %v", err)
			}
		}
	}
	// Use a direct write to stderr for the final message to avoid logging buffers.
	fmt.Fprintln(os.Stderr, "[SHUTDOWN] Shutdown complete.")

	return nil
}

// --- MetricsProvider Interface Implementation ---

// GetConfigForArchive safely retrieves the main config content and its dependencies for archiving.
func (p *Processor) GetConfigForArchive() ([]byte, time.Time, map[string]*types.FileDependency, string, error) {
	p.ConfigMutex.RLock()
	defer p.ConfigMutex.RUnlock()

	// Create a deep copy of the dependencies to avoid race conditions if the config is reloaded
	// while the archive is being generated in a goroutine.
	depsCopy := make(map[string]*types.FileDependency)
	for path, dep := range p.Config.FileDependencies {
		// We only include files that are currently loaded and exist.
		if dep.CurrentStatus != nil && dep.CurrentStatus.Status == types.FileStatusLoaded {
			depsCopy[path] = dep.Clone()
		}
	}

	return p.Config.YAMLContent, p.Config.LastModTime, depsCopy, p.ConfigPath, nil
}

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
							p.LogFunc(logging.LevelInfo, "SHUTDOWN", "Compaction goroutine shutting down.")
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
