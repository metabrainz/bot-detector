//go:build !test

package main

import (
	"bot-detector/internal/logging"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// main is the application entry point.
func main() {
	// Parse CLI flags
	flag.Parse()

	// Validate that required flags are provided.
	if LogFilePath == "" || YAMLFilePath == "" {
		flag.Usage()
		os.Exit(1)
	}
	// Load initial configuration from YAML.
	loadedCfg, err := LoadConfigFromYAML()
	if err != nil {
		log.Fatalf("[FATAL] Configuration Load Error: %v", err)
	}

	logging.SetLogLevel(loadedCfg.LogLevel)

	// Create the config struct from the loaded data.
	appConfig := &AppConfig{
		BlockTableNameFallback: loadedCfg.BlockTableNameFallback,
		CleanupInterval:        loadedCfg.CleanupInterval,
		DefaultBlockDuration:   loadedCfg.DefaultBlockDuration,
		DurationToTableName:    loadedCfg.DurationToTableName,
		EOFPollingDelay:        loadedCfg.EOFPollingDelay,
		FileDependencies:       loadedCfg.FileDependencies,
		HAProxyAddresses:       loadedCfg.HAProxyAddresses,
		HAProxyDialTimeout:     loadedCfg.HAProxyDialTimeout,
		HAProxyMaxRetries:      loadedCfg.HAProxyMaxRetries,
		HAProxyRetryDelay:      loadedCfg.HAProxyRetryDelay,
		IdleTimeout:            loadedCfg.IdleTimeout,
		LastModTime:            time.Now(),
		MaxTimeSinceLastHit:    loadedCfg.MaxTimeSinceLastHit,
		OutOfOrderTolerance:    loadedCfg.OutOfOrderTolerance,
		PollingInterval:        loadedCfg.PollingInterval,
		TimestampFormat:        loadedCfg.TimestampFormat,
		StatFunc:               defaultStatFunc, // Initialize StatFunc to prevent nil pointer panic.
		WhitelistNets:          loadedCfg.WhitelistNets,
	}

	// Initialize the Processor instance.
	p := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ConfigMutex:   &sync.RWMutex{},
		Chains:        loadedCfg.Chains,
		CommandExecutor: func(p *Processor, addr, ip, command string) error {
			return executeCommandImpl(p, addr, ip, command)
		},
		Config:   appConfig,
		LogRegex: loadedCfg.LogFormatRegex,
		DryRun:   DryRun,
		signalCh: make(chan os.Signal, 1),
		LogFunc:  logging.LogOutput,
	}
	// Inject the HAProxyBlocker which depends on the main processor instance.
	p.Blocker = &HAProxyBlocker{P: p}
	p.IsWhitelistedFunc = func(ipInfo IPInfo) bool { return IsIPWhitelisted(p, ipInfo) } // Set the method correctly.
	p.CheckChainsFunc = func(entry *LogEntry) { CheckChains(p, entry) }                  // Assign the real method to the function field.

	// Assign the real implementation for ProcessLogLine.
	p.ProcessLogLine = func(line string, lineNumber int) { processLogLineInternal(p, line, lineNumber) }

	// Log the initial configuration summary.
	logConfigurationSummary(p)
	// At startup, log details for all loaded chains.
	logChainDetails(p, p.Chains, "Loaded chains")

	start(p)
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
		if !isTesting() {
			// The ConfigWatcher is not started in test mode to prevent race conditions
			// where the test's config is overwritten by a reload from the default chains.yaml.
			go ConfigWatcher(p, stopWatcher, nil, nil)
			go CleanUpIdleActivity(p, stopWatcher)
		}
		// Listen for OS signals on the processor's channel
		signal.Notify(p.signalCh, syscall.SIGINT, syscall.SIGTERM)

		// LiveLogTailer is the blocking main loop
		LiveLogTailer(p, p.signalCh, nil) // Pass the processor's channel to the tailer
	}
}
