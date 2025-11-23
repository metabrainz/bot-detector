package metrics

import (
	"sync"
	"sync/atomic"
)

// Metrics holds all the counters for application-level metrics.
// This is designed to be easily adaptable to a Prometheus backend in the future.
type Metrics struct {
	// Per-chain metrics are stored in sync.Maps for thread-safe access.
	ChainsCompleted     *sync.Map
	ChainsReset         *sync.Map
	ChainsHits          *sync.Map
	MatchKeyHits        *sync.Map
	GoodActorHits       *sync.Map
	SkipsByReason       *sync.Map
	BlockDurations      *sync.Map
	CmdsPerBlocker      *sync.Map
	StepExecutionCounts *sync.Map // New: Counter for each logical step executed.
	MetricsEnabled      bool      // New: Flag to enable/disable metric collection.

	// General counters with struct tags for metadata.
	// `metric:"..."` is the display name.
	// `dryrun:"true"` marks it for display in dry-run mode.
	LinesProcessed    atomic.Int64 `metric:"Lines Processed" dryrun:"true"`
	EntriesChecked    atomic.Int64 `metric:"Entries Checked" dryrun:"true"`
	ParseErrors       atomic.Int64 `metric:"Parse Errors" dryrun:"true"`
	GoodActorsSkipped atomic.Int64 `metric:"Good Actors Skipped" dryrun:"true"`
	ReorderedEntries  atomic.Int64 `metric:"Reordered Entries" dryrun:"true"`

	ActorsCleaned       atomic.Int64 `metric:"Actors Cleaned" dryrun:"true"`
	BlockActions        atomic.Int64 `metric:"Block Actions Triggered" dryrun:"true"`
	LogActions          atomic.Int64 `metric:"Log Actions Triggered" dryrun:"true"`
	BlockerCmdsQueued   atomic.Int64 `metric:"Blocker Commands Queued" dryrun:"false"`
	BlockerCmdsDropped  atomic.Int64 `metric:"Blocker Commands Dropped" dryrun:"false"`
	BlockerCmdsExecuted atomic.Int64 `metric:"Blocker Commands Executed" dryrun:"false"`
	BlockerRetries      atomic.Int64 `metric:"Blocker Retries" dryrun:"false"`
	BackendResyncs      atomic.Int64 `metric:"Backend Resyncs Triggered" dryrun:"false"`
	BackendRestarts     atomic.Int64 `metric:"Backend Restarts Detected" dryrun:"false"`
	BackendRecoveries   atomic.Int64 `metric:"Backend Recoveries" dryrun:"false"`
}

// NewMetrics initializes a new Metrics struct.
func NewMetrics() *Metrics {
	return &Metrics{
		// sync.Map is used here as it's optimized for write-once, read-many scenarios
		// and is safe for concurrent access without a global lock, which is ideal
		// for initializing chain counters at startup and incrementing them later.
		ChainsCompleted:     &sync.Map{},
		ChainsReset:         &sync.Map{},
		ChainsHits:          &sync.Map{},
		MatchKeyHits:        &sync.Map{},
		GoodActorHits:       &sync.Map{},
		SkipsByReason:       &sync.Map{},
		BlockDurations:      &sync.Map{},
		CmdsPerBlocker:      &sync.Map{},
		StepExecutionCounts: &sync.Map{}, // Initialize the new field
		MetricsEnabled:      false,       // Initialize to false by default
	}
}
