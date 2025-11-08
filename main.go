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

	// Load initial configuration from YAML.
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
		BlockTableNameFallback: loadedCfg.BlockTableNameFallback,
		CleanupInterval:        loadedCfg.CleanupInterval,
		DurationToTableName:    loadedCfg.DurationToTableName,
		HAProxyAddresses:       loadedCfg.HAProxyAddresses,
		HAProxyDialTimeout:     loadedCfg.HAProxyDialTimeout,
		HAProxyMaxRetries:      loadedCfg.HAProxyMaxRetries,
		HAProxyRetryDelay:      loadedCfg.HAProxyRetryDelay,
		IdleTimeout:            loadedCfg.IdleTimeout,
		LastModTime:            time.Now(),
		MaxTimeSinceLastHit:    loadedCfg.MaxTimeSinceLastHit,
		OutOfOrderTolerance:    loadedCfg.OutOfOrderTolerance,
		PollingInterval:        loadedCfg.PollingInterval,
		WhitelistNets:          loadedCfg.WhitelistNets,
	}

	// Initialize the Processor instance.
	p := &Processor{
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ActivityMutex: &sync.RWMutex{},
		Chains:        loadedCfg.Chains,
		ChainMutex:    &sync.RWMutex{},
		DryRun:        DryRun,
		LogFunc:       LogOutput,
		CommandExecutor: func(p *Processor, addr, ip, command string) error {
			return executeCommandImpl(p, addr, ip, command)
		},
		Config: appConfig,
	}
	// Inject the HAProxyBlocker which depends on the main processor instance.
	p.Blocker = &HAProxyBlocker{P: p}
	p.IsWhitelistedFunc = p.IsIPWhitelisted // Set the method correctly.

	// Assign the real implementation for ProcessLogLine.
	p.ProcessLogLine = func(line string, lineNumber int) { processLogLineInternal(p, line, lineNumber) }

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

		go p.ChainWatcher(stopWatcher, nil, nil) // Pass nil for test-only channels
		go p.CleanUpIdleActivity(stopWatcher)

		// Set up signal handling for graceful shutdown in live mode.
		signalCh := make(chan os.Signal, 1)
		signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

		// LiveLogTailer is the blocking main loop
		LiveLogTailer(p, signalCh, nil)
	}
}
