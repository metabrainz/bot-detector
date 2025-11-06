package main

import (
	"time"
)

// resetGlobalState is a helper function to reset all global mutable state
// (runtime stores and configuration settings) for isolated testing.
func resetGlobalState() {
	// Reset runtime stores
	ActivityMutex.Lock()
	ActivityStore = make(map[TrackingKey]*BotActivity)
	ActivityMutex.Unlock()

	DryRunActivityMutex.Lock()
	DryRunActivityStore = make(map[TrackingKey]*BotActivity)
	DryRunActivityMutex.Unlock()

	// Reset config-related chains and whitelist
	ChainMutex.Lock()
	Chains = nil
	WhitelistNets = nil
	LastModTime = time.Time{}
	ChainMutex.Unlock()

	// Reset HAProxy addresses
	HAProxyMutex.Lock()
	HAProxyAddresses = nil
	HAProxyMutex.Unlock()

	// Reset Duration Tables
	DurationTableMutex.Lock()
	DurationToTableName = nil
	BlockTableNameFallback = ""
	DurationTableMutex.Unlock()

	// Reset log level
	CurrentLogLevel = LevelWarning
	// Reset global testing flags
	DryRun = false
	LogLevelStr = ""
	PollingIntervalStr = ""
	CleanupIntervalStr = ""
	IdleTimeoutStr = ""
}
