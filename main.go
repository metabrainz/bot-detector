package main

import (
	"flag"
	"log"
)

// main is the application entry point.
func main() {
	// Parse CLI flags
	flag.Parse()

	// Validate and parse duration strings (e.g., "5m", "10s")
	if err := ParseDurations(); err != nil {
		log.Fatalf("[FATAL] Configuration Error: %v", err)
	}

	// Load initial configuration
	if _, err := LoadChainsFromYAML(); err != nil {
		log.Fatalf("[FATAL] Configuration Load Error: %v", err)
	}

	// Execute the core application logic
	start()
}

// start is the unexported function that contains the main application logic,
// which is called by the tests and the main function.
func start() {
	if DryRun {
		// DryRun mode: Process a static log file and exit when done.
		done := make(chan struct{})
		go DryRunLogProcessor(done)

		// Wait for the processor to finish in dry-run mode
		<-done

	} else {
		// Live mode: Start background routines and the main log tailing loop.
		go ChainWatcher()
		go CleanUpIdleActivity()

		// LiveLogTailer is the blocking main loop
		LiveLogTailer()
	}
}
