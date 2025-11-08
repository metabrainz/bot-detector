//go:build !test

package main

import (
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

	// Load initial configuration from YAML. This no longer sets global state.
	loadedCfg, err := LoadChainsFromYAML()
	if err != nil {
		log.Fatalf("[FATAL] Configuration Load Error: %v", err)
	}

	SetLogLevel(loadedCfg.LogLevel)

	if len(loadedCfg.DurationToTableName) == 0 {
		LogOutput(LevelWarning, "CONFIG", "No HAProxy duration tables configured. All block attempts will be skipped.")
	}

	// Create the config struct from the loaded data.
	appConfig := &AppConfig{
		WhitelistNets:          loadedCfg.WhitelistNets,
		HAProxyAddresses:       loadedCfg.HAProxyAddresses,
		HAProxyMaxRetries:      loadedCfg.HAProxyMaxRetries,
		HAProxyRetryDelay:      loadedCfg.HAProxyRetryDelay,
		HAProxyDialTimeout:     loadedCfg.HAProxyDialTimeout,
		DurationToTableName:    loadedCfg.DurationToTableName,
		BlockTableNameFallback: loadedCfg.BlockTableNameFallback,
		LastModTime:            time.Now(),
		PollingInterval:        loadedCfg.PollingInterval,
		IdleTimeout:            loadedCfg.IdleTimeout,
		CleanupInterval:        loadedCfg.CleanupInterval,
		OutOfOrderTolerance:    loadedCfg.OutOfOrderTolerance,
		MaxTimeSinceLastHit:    loadedCfg.MaxTimeSinceLastHit,
	}

	// Initialize the global Processor instance after config is loaded.
	// This centralizes dependency injection for the entire application.
	// We use the global state variables (ActivityStore, Chains, etc.) to
	// populate the single Processor instance.
	processor := &Processor{
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ActivityMutex: &sync.RWMutex{},
		Chains:        loadedCfg.Chains,
		ChainMutex:    &sync.RWMutex{},
		DryRun:        DryRun,
		LogFunc:       LogOutput,
		// Blocker will be set below
		CommandExecutor: func(p *Processor, addr, ip, command string) error {
			return executeCommandImpl(p, addr, ip, command)
		},
		Config: appConfig,
	}
	// Inject the HAProxyBlocker which depends on the main processor instance.
	processor.Blocker = &HAProxyBlocker{P: processor}
	processor.IsWhitelistedFunc = processor.IsIPWhitelisted // Set the method correctly.
	// Switch to the DryRun store/mutex if running in dry-run mode
	if DryRun {
		processor.ActivityStore = make(map[TrackingKey]*BotActivity)
		// The mutex is the same, just the store is different.
	}

	// Execute the core application logic
	// Assign the real implementation for ProcessLogLine.
	processor.ProcessLogLine = processor.processLogLineInternal

	start(processor)
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

		go p.ChainWatcher(stopWatcher)
		go p.CleanUpIdleActivity(stopWatcher)

		// Set up signal handling for graceful shutdown in live mode.
		signalCh := make(chan os.Signal, 1)
		signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

		// LiveLogTailer is the blocking main loop
		LiveLogTailer(p, signalCh)
	}
}
