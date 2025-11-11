package main

import (
	"sync"
	"sync/atomic"
)

// Metrics holds all the counters for application-level metrics.
// This is designed to be easily adaptable to a Prometheus backend in the future.
type Metrics struct {
	// Per-chain metrics are stored in sync.Maps for thread-safe access.
	ChainsCompleted *sync.Map
	ChainsReset     *sync.Map

	// General counters with struct tags for metadata.
	// `metric:"..."` is the display name.
	// `dryrun:"true"` marks it for display in dry-run mode.
	LinesProcessed      atomic.Int64 `metric:"Lines Processed" dryrun:"true"`
	ParseErrors         atomic.Int64 `metric:"Parse Errors" dryrun:"true"`
	ReorderedEntries    atomic.Int64 `metric:"Reordered Entries" dryrun:"true"`
	WhitelistedHits     atomic.Int64 `metric:"Whitelisted Hits Skipped" dryrun:"true"`
	ActivitiesCleaned   atomic.Int64 `metric:"Activities Cleaned" dryrun:"true"`
	BlockActions        atomic.Int64 `metric:"Block Actions Triggered" dryrun:"true"`
	LogActions          atomic.Int64 `metric:"Log Actions Triggered" dryrun:"true"`
	BlockerCmdsQueued   atomic.Int64 `metric:"Blocker Commands Queued" dryrun:"false"`
	BlockerCmdsDropped  atomic.Int64 `metric:"Blocker Commands Dropped" dryrun:"false"`
	BlockerCmdsExecuted atomic.Int64 `metric:"Blocker Commands Executed" dryrun:"false"`
	BlockerRetries      atomic.Int64 `metric:"Blocker Retries" dryrun:"false"`
}

// NewMetrics initializes a new Metrics struct.
func NewMetrics() *Metrics {
	return &Metrics{
		// sync.Map is used here as it's optimized for write-once, read-many scenarios
		// and is safe for concurrent access without a global lock, which is ideal
		// for initializing chain counters at startup and incrementing them later.
		ChainsCompleted: &sync.Map{},
		ChainsReset:     &sync.Map{},
	}
}
