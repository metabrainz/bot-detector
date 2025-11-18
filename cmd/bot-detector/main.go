package main

import (
	"bot-detector/internal/app"
	"bot-detector/internal/blocker"
	"bot-detector/internal/checker"
	"bot-detector/internal/cluster"
	"bot-detector/internal/commandline"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/logparser"
	"bot-detector/internal/metrics"
	"bot-detector/internal/persistence"
	"bot-detector/internal/processor"
	"bot-detector/internal/server"
	"bot-detector/internal/store"
	"bot-detector/internal/testutil"
	"bot-detector/internal/utils"
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

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
		switch err.Error() {
		case "flag: help requested", "no flag: help requested":
			os.Exit(0)
		default:
			// A parsing error will have already printed usage information.
			// We exit with a non-zero code after the error is logged.
			log.Printf("[FATAL] %v", err)
			os.Exit(1)
		}
	}

	if err := execute(params); err != nil {
		if err.Error() != "exit" {
			log.Printf("[FATAL] %v", err)
			os.Exit(1)
		} else {
			os.Exit(0)
		}
	}
}

func GetConfigFilePath(params *commandline.AppParameters) string {
	return filepath.Join(params.ConfigDir, "config.yaml")
}

// handleStartupFlags checks for command-line flags that prevent normal startup,
// such as --version or --check. It returns a special "exit" error to signal
// a clean exit, an error for failures, or nil to continue execution.
func handleStartupFlags(params *commandline.AppParameters) error {
	if params.ShowVersion {
		fmt.Printf("bot-detector version %s\n", config.AppVersion)
		return fmt.Errorf("exit") // Signal clean exit
	}

	if params.Check {
		opts := config.LoadConfigOptions{
			ConfigFilePath: GetConfigFilePath(params),
		}
		var err error
		if _, err = config.LoadConfigFromYAML(opts); err != nil {
			return fmt.Errorf("configuration check failed: %v", err)
		}
		log.Println("[SUCCESS] Configuration is valid.")
		return fmt.Errorf("exit") // Signal clean exit
	}

	return nil // Continue execution
}

func initializeProcessor(params *commandline.AppParameters, appConfig *config.AppConfig, loadedCfg *config.LoadedConfig) *app.Processor {
	return &app.Processor{
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
		EnableMetrics:        loadedCfg.Application.EnableMetrics,
		OooBufferFlushSignal: make(chan struct{}, 1), // Buffered channel of size 1
		SignalCh:             make(chan os.Signal, 1),
		LogFunc:              logging.LogOutput,
		NowFunc:              time.Now, // Use the real time.Now in production.
		ConfigFilePath:       GetConfigFilePath(params),
		ConfigDir:            params.ConfigDir,
		LogPath:              params.LogPath,
		ReloadOn:             params.ReloadOn,
		TopN:                 params.TopN,
		HTTPServer:           params.HTTPServer,
		ConfigReloaded:       false,

		// Initialize persistence fields
		PersistenceEnabled: loadedCfg.Application.Persistence.Enabled,
		CompactionInterval: loadedCfg.Application.Persistence.CompactionInterval,
		ActiveBlocks:       make(map[string]persistence.ActiveBlockInfo),

		// Initialize cluster fields with defaults (will be set properly in later phases)
		Cluster:           loadedCfg.Cluster,
		NodeRole:          "leader",
		NodeName:          "",
		NodeAddress:       "",
		NodeLeaderAddress: "",
	}
}

func restorePersistenceState(p *app.Processor) error {
	// -- STATE RESTORATION --
	if err := os.MkdirAll(p.StateDir, 0750); err != nil {
		return fmt.Errorf("failed to create state directory '%s': %v", p.StateDir, err)
	}
	p.LogFunc(logging.LevelInfo, "SETUP", "Persistence enabled. Loading state from '%s'...", p.StateDir)

	// 1. Load snapshot
	snapshot, err := persistence.LoadSnapshot(filepath.Join(p.StateDir, "state.snapshot"))
	if err != nil {
		p.LogFunc(logging.LevelError, "STATE_LOAD_FAIL", "Failed to load snapshot: %v", err)
		return err
	}
	p.ActiveBlocks = snapshot.ActiveBlocks

	// 2. Replay Journal
	journalPath := filepath.Join(p.StateDir, "events.log")
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
					p.ActiveBlocks[event.IP] = persistence.ActiveBlockInfo{
						UnblockTime: event.Timestamp.Add(event.Duration),
						Reason:      event.Reason,
					}
				case persistence.EventTypeUnblock:
					delete(p.ActiveBlocks, event.IP)
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
	p.LogFunc(logging.LevelInfo, "STATE_RESTORE", "Restoring %d active blocks to backend...", len(p.ActiveBlocks))
	// Create a sorted list of table durations for best-fit matching
	type tableInfo struct {
		duration time.Duration
		name     string
	}
	var sortedTables []tableInfo
	for d, n := range p.Config.Blockers.Backends.HAProxy.DurationTables {
		sortedTables = append(sortedTables, tableInfo{duration: d, name: n})
	}
	sort.Slice(sortedTables, func(i, j int) bool {
		return sortedTables[i].duration < sortedTables[j].duration
	})

	for ip, info := range p.ActiveBlocks {
		// Before restoring, check if the IP is now a good actor.
		tempEntry := &app.LogEntry{IPInfo: utils.NewIPInfo(ip)}
		if isGood, reason := checker.IsGoodActor(p, tempEntry); isGood {
			p.LogFunc(logging.LevelInfo, "STATE_RESTORE_SKIP", "Skipping restore for %s (good_actor: %s)", ip, reason)
			continue // Don't restore blocks for good actors.
		}

		remainingDuration := time.Until(info.UnblockTime)
		if remainingDuration > 0 {
			bestFitDuration := p.Config.Blockers.DefaultDuration
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
	p.JournalHandle, err = persistence.OpenJournalForAppend(journalPath)
	if err != nil {
		p.LogFunc(logging.LevelError, "JOURNAL_OPEN_FAIL", "Failed to open journal for writing: %v", err)
		return err
	}
	return nil
}

// execute is the main application logic, decoupled from command-line parsing.
func execute(params *commandline.AppParameters) error {
	if err := handleStartupFlags(params); err != nil {
		if err.Error() == "exit" {
			return nil
		}
		return err
	}

	configFilePath := GetConfigFilePath(params)

	// Check if FOLLOW file exists and determine if we need to bootstrap
	followPath := filepath.Join(params.ConfigDir, "FOLLOW")
	followData, err := os.ReadFile(followPath)
	if err == nil {
		// FOLLOW file exists - this is a follower
		leaderAddr := strings.TrimSpace(string(followData))
		if leaderAddr == "" {
			return fmt.Errorf("FOLLOW file exists but is empty")
		}

		// Check if config file exists, if not, bootstrap
		if _, err := os.Stat(configFilePath); os.IsNotExist(err) {
			// Config doesn't exist, bootstrap from leader
			// Add http:// prefix if not present
			if !strings.HasPrefix(leaderAddr, "http://") && !strings.HasPrefix(leaderAddr, "https://") {
				leaderAddr = "http://" + leaderAddr
			}

			logging.LogOutput(logging.LevelInfo, "CLUSTER", "Config file not found, bootstrapping from leader at %s", leaderAddr)
			if err := cluster.Bootstrap(cluster.BootstrapOptions{
				LeaderAddress:  leaderAddr,
				ConfigFilePath: configFilePath,
				LogFunc:        logging.LogOutput,
				HTTPTimeout:    10 * time.Second,
				ForceUpdate:    false,
			}); err != nil {
				return fmt.Errorf("failed to bootstrap config from leader: %w", err)
			}
		}
	} else if !os.IsNotExist(err) {
		// Error reading FOLLOW file (but not "file doesn't exist")
		return fmt.Errorf("failed to read FOLLOW file: %w", err)
	}
	// If FOLLOW doesn't exist, this is a leader - no bootstrap needed

	fileInfo, err := os.Stat(configFilePath)
	if err != nil {
		return fmt.Errorf("could not stat file: %q  - %v", configFilePath, err)
	}

	loadedCfg, err := config.LoadConfigFromYAML(config.LoadConfigOptions{ConfigFilePath: configFilePath})
	if err != nil {
		return fmt.Errorf("configuration Load Error: %v", err)
	}

	logging.SetLogLevel(loadedCfg.Application.LogLevel)

	appConfig := &config.AppConfig{
		Application:      loadedCfg.Application,
		Parser:           loadedCfg.Parser,
		Checker:          loadedCfg.Checker,
		Blockers:         loadedCfg.Blockers,
		GoodActors:       loadedCfg.GoodActors,
		FileDependencies: loadedCfg.FileDependencies,
		LastModTime:      fileInfo.ModTime(),
		StatFunc:         processor.DefaultStatFunc,
		FileOpener:       func(name string) (config.FileHandle, error) { return os.Open(name) },
		YAMLContent:      loadedCfg.YAMLContent,
	}

	p := initializeProcessor(params, appConfig, loadedCfg)

	if params.StateDir != "" {
		p.StateDir = params.StateDir
	}

	if p.PersistenceEnabled && p.StateDir == "" {
		return fmt.Errorf("persistence is enabled in config, but no --state-dir was provided")
	}

	// Determine node identity based on FOLLOW file and cluster configuration
	identity, err := cluster.DetermineIdentity(params.ConfigDir, params.HTTPServer, loadedCfg.Cluster)
	if err != nil {
		return fmt.Errorf("failed to determine node identity: %w", err)
	}
	p.NodeRole = identity.Role.String()
	p.NodeName = identity.Name
	p.NodeAddress = identity.Address
	p.NodeLeaderAddress = identity.LeaderAddress

	logging.LogOutput(logging.LevelInfo, "CLUSTER", "Node identity: %s", identity.String())

	p.StartTime = p.NowFunc()
	p.SignalOooBufferFlush = func() { checker.DoSignalOooBufferFlush(p) }
	app.InitializeMetrics(p, loadedCfg)

	haproxyBlocker := blocker.NewHAProxyBlocker(p, p.DryRun)
	rateLimitedBlocker := blocker.NewRateLimitedBlocker(p, p, haproxyBlocker, p.Config.Blockers.CommandQueueSize, p.Config.Blockers.CommandsPerSecond)
	p.Blocker = rateLimitedBlocker

	if p.PersistenceEnabled {
		if err := restorePersistenceState(p); err != nil {
			return err
		}
	}

	if params.DumpBackends {
		return dumpBackendsAndExit(p)
	}

	p.CheckChainsFunc = func(entry *app.LogEntry) { checker.CheckChains(p, entry) }
	p.ProcessLogLine = func(line string) { logparser.ProcessLogLineInternal(p, line) }

	app.LogConfigurationSummary(p)
	app.LogChainDetails(p, p.Chains, "Loaded chains")

	start(p)

	performGracefulShutdown(p)

	return nil
}

func dumpBackendsAndExit(p *app.Processor) error {
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

func performGracefulShutdown(p *app.Processor) {
	p.LogFunc(logging.LevelInfo, "SHUTDOWN", "Graceful shutdown initiated.")

	if p.Blocker != nil {
		p.Blocker.Shutdown()
	}

	if p.PersistenceEnabled {
		p.LogFunc(logging.LevelInfo, "PERSISTENCE", "Waiting for persistence operations to complete...")
		p.PersistenceWg.Wait()

		if p.CompactionInterval > 0 {
			p.LogFunc(logging.LevelInfo, "SHUTDOWN", "Performing final state compaction...")
			runCompaction(p)
		}

		p.LogFunc(logging.LevelInfo, "PERSISTENCE", "Closing journal file.")
		if p.JournalHandle != nil {
			if err := p.JournalHandle.Close(); err != nil {
				p.LogFunc(logging.LevelError, "PERSISTENCE", "Error closing journal file: %v", err)
			}
		}
	}
	fmt.Fprintln(os.Stderr, "[SHUTDOWN] Shutdown complete.")
}

func runCompaction(p *app.Processor) {
	p.PersistenceMutex.Lock()
	defer p.PersistenceMutex.Unlock()

	// Filter out expired blocks before snapshotting
	now := p.NowFunc()
	activeBlocks := make(map[string]persistence.ActiveBlockInfo)
	for ip, info := range p.ActiveBlocks {
		if now.Before(info.UnblockTime) {
			activeBlocks[ip] = info
		}
	}
	p.ActiveBlocks = activeBlocks // Update in-memory map

	snapshot := &persistence.Snapshot{
		Timestamp:    now,
		ActiveBlocks: p.ActiveBlocks,
	}

	if err := persistence.WriteSnapshot(filepath.Join(p.StateDir, "state.snapshot"), snapshot); err != nil {
		p.LogFunc(logging.LevelError, "COMPACTION_FAIL", "Failed to write snapshot: %v", err)
		return
	}

	// Truncate and re-open journal
	journalPath := filepath.Join(p.StateDir, "events.log")
	if err := p.JournalHandle.Close(); err != nil {
		p.LogFunc(logging.LevelError, "COMPACTION_FAIL", "Failed to close journal for truncation: %v", err)
	}

	// Re-open with truncation
	handle, err := os.OpenFile(journalPath, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		p.LogFunc(logging.LevelError, "COMPACTION_FAIL", "Failed to truncate and re-open journal: %v", err)
		// Attempt to re-open in append mode as a fallback
		handle, _ = persistence.OpenJournalForAppend(journalPath)
	}
	p.JournalHandle = handle
	p.LogFunc(logging.LevelInfo, "COMPACTION", "State snapshot and journal compaction completed successfully.")
}

// start is the unexported function that contains the main application logic,
// which is called by the tests and the main function.
func start(p *app.Processor) {
	if p.DryRun {
		// DryRun mode: Process a static log file and exit when done.
		done := make(chan struct{})
		// Pass the Processor instance P
		go processor.DryRunLogProcessor(p, done)

		// Wait for the processor to finish in dry-run mode
		<-done

	} else {
		// Live mode: Start background routines and the main log tailing loop.
		stopWatcher := make(chan struct{})
		defer close(stopWatcher) // Ensure watcher is stopped on main exit
		// Only start these background tasks if not in a test that bypasses them.
		// This allows tests to focus on specific components like the tailer without
		// interference from other goroutines like the config watcher.
		if !testutil.IsTesting() {
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
				go app.SignalReloader(p, stopWatcher, reloadSignalCh)

			case "watcher":
				// Flag is 'watcher'. Signal reloading is disabled.
				p.LogFunc(logging.LevelInfo, "SETUP", "Signal-based reloading disabled. Watching config file for changes.")
				go app.ConfigWatcher(p, stopWatcher)

			case "":
				// Flag is absent. Both watcher and SIGHUP reloader are active.
				p.LogFunc(logging.LevelInfo, "SETUP", "File watcher enabled. Also listening on SIGHUP for forced reload.")
				go app.ConfigWatcher(p, stopWatcher)
				go app.SignalReloader(p, stopWatcher, reloadSignalCh) // This will default to HUP when p.ReloadOn is ""
			default:
				// An invalid value was provided. Log a fatal error and exit.
				log.Fatalf("[FATAL] Invalid value for --reload-on: '%s'. Must be one of 'watcher', 'hup', 'usr1', or 'usr2'.", p.ReloadOn)
			}

			if p.PersistenceEnabled && p.CompactionInterval > 0 {
				go func() {
					ticker := time.NewTicker(p.CompactionInterval)
					defer ticker.Stop()
					p.LogFunc(logging.LevelInfo, "SETUP", "State compaction enabled, running every %v.", p.CompactionInterval)
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
				go processor.CleanupTopActors(p, stopWatcher)
			}
			go store.CleanUpIdleActors(p, stopWatcher)
			go checker.EntryBufferWorker(p, stopWatcher)
			go server.Start(p)

			// Start config poller for follower nodes
			if p.NodeRole == "follower" && p.NodeLeaderAddress != "" {
				// Create a channel for config reload signals
				configReloadCh := make(chan struct{}, 1)

				// Start the config poller
				pollInterval := 30 * time.Second // Default
				if p.Cluster != nil && p.Cluster.ConfigPollInterval > 0 {
					pollInterval = p.Cluster.ConfigPollInterval
				}

				poller := cluster.NewConfigPoller(cluster.ConfigPollerOptions{
					LeaderAddress:  p.NodeLeaderAddress,
					ConfigFilePath: p.ConfigFilePath,
					PollInterval:   pollInterval,
					ConfigReloadCh: configReloadCh,
					ShutdownCh:     p.SignalCh,
					LogFunc:        p.LogFunc,
					HTTPTimeout:    10 * time.Second,
				})
				go poller.Start()

				// Handle config reload signals
				go func() {
					for {
						select {
						case <-configReloadCh:
							p.LogFunc(logging.LevelInfo, "CLUSTER", "Config reload triggered by leader update")
							// Make a copy of the old config for comparison
							p.ConfigMutex.RLock()
							oldConfig := p.Config.Clone()
							p.ConfigMutex.RUnlock()
							// Trigger config reload
							app.ReloadConfiguration(p, true, &oldConfig)
						case <-stopWatcher:
							return
						}
					}
				}()
			}

			// Start metrics collector for leader nodes
			if p.NodeRole == "leader" && p.Cluster != nil && len(p.Cluster.Nodes) > 0 {
				metricsInterval := 60 * time.Second // Default
				if p.Cluster.MetricsReportInterval > 0 {
					metricsInterval = p.Cluster.MetricsReportInterval
				}

				collector := cluster.NewMetricsCollector(cluster.MetricsCollectorOptions{
					Nodes:        p.Cluster.Nodes,
					PollInterval: metricsInterval,
					ShutdownCh:   p.SignalCh,
					LogFunc:      p.LogFunc,
					HTTPTimeout:  10 * time.Second,
					Protocol:     p.Cluster.Protocol,
				})
				// Store collector reference in processor for access by HTTP handlers
				p.MetricsCollector = collector
				go collector.Start()
			}
		}
		// Listen for OS signals on the processor's channel
		signal.Notify(p.SignalCh, syscall.SIGINT, syscall.SIGTERM)

		// LiveLogTailer is the blocking main loop
		processor.LiveLogTailer(p, p.SignalCh, nil) // Pass the processor's channel to the tailer
	}
}
