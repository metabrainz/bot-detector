package main

import (
	"sync"
	"sync/atomic"
)

// Metrics holds all the counters for application-level metrics.
// This is designed to be easily adaptable to a Prometheus backend in the future.
type Metrics struct {
	// A map to store counters for each completed chain, keyed by chain name.
	// This maps directly to a Prometheus counter with a 'chain' label.
	ChainsCompleted *sync.Map

	ChainsReset *sync.Map
	// General counters.
	ParseErrors      atomic.Int64
	ReorderedEntries atomic.Int64
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
