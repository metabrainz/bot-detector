package main

import (
	"flag"
	"log"
)

// P is the global application Processor instance holding all state and dependencies.
// All core logic will be called via this instance.
var P *Processor

// main is the application entry point.
func main() {
	// Parse CLI flags
	flag.Parse()

	// Validate and parse duration strings (e.g., "5m", "10s")
	if err := ParseDurations(); err != nil {
		log.Fatalf("[FATAL] Configuration Error: %v", err)
	}

	// Load initial configuration
	// Note: LoadChainsFromYAML updates the global state variables (Chains, WhitelistNets, etc.)
	if _, err := LoadChainsFromYAML(); err != nil {
		log.Fatalf("[FATAL] Configuration Load Error: %v", err)
	}

	// Create the initial config struct from the loaded global state.
	// This is a transitional step. Eventually, LoadChainsFromYAML will return this directly.
	appConfig := &AppConfig{
		WhitelistNets:          WhitelistNets,
		HAProxyAddresses:       HAProxyAddresses,
		DurationToTableName:    DurationToTableName,
		BlockTableNameFallback: BlockTableNameFallback,
		LastModTime:            LastModTime,
	}

	// Initialize the global Processor instance after config is loaded.
	// This centralizes dependency injection for the entire application.
	// We use the global state variables (ActivityStore, Chains, etc.) to
	// populate the single Processor instance.
	P = &Processor{
		ActivityStore: ActivityStore,
		ActivityMutex: &ActivityMutex,
		Chains:        Chains,
		ChainMutex:    &ChainMutex,
		DryRun:        DryRun,
		LogFunc:       LogOutput,
		Blocker:       &GlobalBlocker{},
		Config:        appConfig,
	}
	P.IsWhitelistedFunc = P.IsIPWhitelisted // Set the method correctly.
	// Switch to the DryRun store/mutex if running in dry-run mode
	if DryRun {
		P.ActivityStore = DryRunActivityStore
		P.ActivityMutex = &DryRunActivityMutex
	}

	// Execute the core application logic
	start(P)
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
		// Pass the Processor instance P to all background routines
		go ChainWatcher(p)
		go CleanUpIdleActivity(p)

		// LiveLogTailer is the blocking main loop
		LiveLogTailer(p)
	}
}
