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

	// Per-website metrics (multi-website mode)
	WebsiteLinesParsed    *sync.Map // map[website]int64
	WebsiteChainsMatched  *sync.Map // map[website]int64
	WebsiteChainsReset    *sync.Map // map[website]int64
	WebsiteChainsComplete *sync.Map // map[website]int64

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
	ChallengeActions    atomic.Int64 `metric:"Challenge Actions Triggered" dryrun:"true"`
	BlockerCmdsQueued   atomic.Int64 `metric:"Blocker Commands Queued" dryrun:"false"`
	BlockerCmdsDropped  atomic.Int64 `metric:"Blocker Commands Dropped" dryrun:"false"`
	BlockerCmdsExecuted atomic.Int64 `metric:"Blocker Commands Executed" dryrun:"false"`
	BlockerRetries      atomic.Int64 `metric:"Blocker Retries" dryrun:"false"`
	BackendResyncs      atomic.Int64 `metric:"Backend Resyncs Triggered" dryrun:"false"`
	BackendRestarts     atomic.Int64 `metric:"Backend Restarts Detected" dryrun:"false"`
	BackendRecoveries   atomic.Int64 `metric:"Backend Recoveries" dryrun:"false"`

	RecentParseErrors *ParseErrorBuffer // Ring buffer of recent parse error messages
}

// ParseErrorBuffer is a thread-safe ring buffer that stores the most recent parse errors.
type ParseErrorBuffer struct {
	mu      sync.Mutex
	entries []string
	pos     int
	cap     int
	full    bool
}

// NewParseErrorBuffer creates a ring buffer of the given capacity. Returns nil if cap <= 0.
func NewParseErrorBuffer(cap int) *ParseErrorBuffer {
	if cap <= 0 {
		return nil
	}
	return &ParseErrorBuffer{entries: make([]string, cap), cap: cap}
}

// Add stores an error string in the ring buffer.
func (b *ParseErrorBuffer) Add(s string) {
	b.mu.Lock()
	b.entries[b.pos] = s
	b.pos = (b.pos + 1) % b.cap
	if b.pos == 0 {
		b.full = true
	}
	b.mu.Unlock()
}

// Entries returns all stored errors, newest first.
func (b *ParseErrorBuffer) Entries() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.pos
	if b.full {
		n = b.cap
	}
	result := make([]string, n)
	for i := range n {
		// Walk backwards from most recent
		idx := (b.pos - 1 - i + b.cap) % b.cap
		result[i] = b.entries[idx]
	}
	return result
}

// NewMetrics initializes a new Metrics struct.
func NewMetrics() *Metrics {
	return &Metrics{
		// sync.Map is used here as it's optimized for write-once, read-many scenarios
		// and is safe for concurrent access without a global lock, which is ideal
		// for initializing chain counters at startup and incrementing them later.
		ChainsCompleted:       &sync.Map{},
		ChainsReset:           &sync.Map{},
		ChainsHits:            &sync.Map{},
		MatchKeyHits:          &sync.Map{},
		GoodActorHits:         &sync.Map{},
		SkipsByReason:         &sync.Map{},
		BlockDurations:        &sync.Map{},
		CmdsPerBlocker:        &sync.Map{},
		StepExecutionCounts:   &sync.Map{}, // Initialize the new field
		MetricsEnabled:        false,       // Initialize to false by default
		WebsiteLinesParsed:    &sync.Map{},
		WebsiteChainsMatched:  &sync.Map{},
		WebsiteChainsReset:    &sync.Map{},
		WebsiteChainsComplete: &sync.Map{},
	}
}

// IncrementWebsiteMetric atomically increments a counter in a website metrics map.
func IncrementWebsiteMetric(m *sync.Map, website string, delta int64) {
	val, _ := m.LoadOrStore(website, new(atomic.Int64))
	val.(*atomic.Int64).Add(delta)
}

// GetWebsiteMetric returns the current value of a website metric.
func GetWebsiteMetric(m *sync.Map, website string) int64 {
	val, ok := m.Load(website)
	if !ok {
		return 0
	}
	return val.(*atomic.Int64).Load()
}
