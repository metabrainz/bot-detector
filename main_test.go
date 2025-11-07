package main

// resetGlobalState is a helper function to reset all global mutable state
// (runtime stores and configuration settings) for isolated testing.
func resetGlobalState() {
	// Reset log level
	CurrentLogLevel = LevelWarning
	// Reset global testing flags
	DryRun = false
}
