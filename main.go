//go:build !test

package main

import (
	"bot-detector/internal/blocker"
	"bot-detector/internal/logging"
	metrics "bot-detector/internal/metrics"
	"bot-detector/internal/server"
	"bot-detector/internal/store"
	"bot-detector/internal/utils"
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
	}
	p.startTime = p.NowFunc() // Record the start time.
	// TestSignals is intentionally left nil in production.
	// Set up the signalOooBufferFlush field to call the method.
	p.signalOooBufferFlush = p.doSignalOooBufferFlush
	initializeMetrics(p, loadedCfg)

	haproxyBlocker := blocker.NewHAProxyBlocker(p, p.DryRun)
	rateLimitedBlocker := blocker.NewRateLimitedBlocker(p, p, haproxyBlocker, p.Config.BlockerCommandQueueSize, p.Config.BlockerCommandsPerSecond)

	p.Blocker = rateLimitedBlocker
	defer rateLimitedBlocker.Stop() // Ensure the rate limiter worker is stopped on exit.

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
