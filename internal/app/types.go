package app

import (
	"bot-detector/internal/blocker"
	"bot-detector/internal/logging"
	metrics "bot-detector/internal/metrics"
	"bot-detector/internal/persistence"
	"bot-detector/internal/store"
	"bot-detector/internal/utils"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

// fileOpener defines the function signature for opening a file, returning our interface.
type fileOpener func(name string) (fileHandle, error)

// fileHandle defines the interface for file operations needed by the tailer.
type fileHandle interface {
	io.ReadSeeker
	io.Closer
	Stat() (os.FileInfo, error)
}

// FieldType indicates the native type of a field from a LogEntry.
type FieldType int

const (
	StringField FieldType = iota
	IntField
	UnsupportedField
)

// TestSignals holds channels used exclusively for test synchronization.
// This struct is nil in production.
type TestSignals struct {
	CleanupDoneSignal chan struct{}
	EOFCheckSignal    chan struct{}
	ReloadDoneSignal  chan struct{}
	ForceCheckSignal  chan struct{}
}

// FieldNameCanonicalMap maps lowercase YAML field names to their canonical PascalCase
// equivalents in the LogEntry struct. This allows for case-insensitive configuration.
var FieldNameCanonicalMap = map[string]string{
	"ip":         "IP",
	"path":       "Path",
	"method":     "Method",
	"protocol":   "Protocol",
	"useragent":  "UserAgent",
	"user_agent": "UserAgent",
	"referrer":   "Referrer",
	"statuscode": "StatusCode",
	"size":       "Size",
	"vhost":      "VHost",
}

// --- DEPENDENCY CONTAINER ---

// Processor holds all necessary dependencies and state for log processing,
// making it easy to mock/stub external calls and manage state in tests.
type Processor struct {
	ActivityMutex *sync.RWMutex
	ActivityStore map[store.Actor]*store.ActorActivity
	Blocker       blocker.Blocker
	ConfigMutex   *sync.RWMutex
	Metrics       *metrics.Metrics
	Chains        []BehavioralChain
	Config        *config.AppConfig
	DryRun        bool
	EnableMetrics bool

	EntryBuffer          []*LogEntry    // Buffer for holding out-of-order entries.
	oooBufferFlushSignal chan struct{}  // Signal to the entryBufferWorker to flush the OOO buffer immediately.
	LogRegex             *regexp.Regexp // The currently active log parsing regex.
	CheckChainsFunc      func(entry *LogEntry)
	signalCh             chan os.Signal
	LogFunc              func(level logging.LogLevel, tag string, format string, v ...interface{})
	ProcessLogLine       func(line string)
	NowFunc              func() time.Time // Mockable time function.
	signalOooBufferFlush func()
	TestSignals          *TestSignals // Test-only signals for synchronization.
	ConfigPath           string
	LogPath              string `test:"-"`
	ReloadOn             string
	TopActorsPerChain    map[string]map[string]*store.ActorStats // Dry-run only: tracks top actors per chain.
	HTTPServer           string
	TopN                 int // For dry-run stats: show top N actors.
	startTime            time.Time
	// Persistence fields
	persistenceEnabled bool
	stateDir           string
	compactionInterval time.Duration
	persistenceMutex   sync.Mutex
	persistenceWg      sync.WaitGroup
	journalHandle      *os.File
	activeBlocks       map[string]persistence.ActiveBlockInfo
	// configReloaded is a flag to indicate if the configuration has been reloaded at least once.
	configReloaded bool
	// ExitOnEOF is a flag to indicate if the tailer should exit when it reaches the end of the file.
	ExitOnEOF bool
}

// AppConfig holds all the configuration state that can be reloaded from YAML.

// Config types moved to internal/config/types.go

type StepDefYAML struct {
	Order               int
	FieldMatches        map[string]interface{} `yaml:"field_matches"`
	MaxDelay            string                 `yaml:"max_delay"`
	MinDelay            string                 `yaml:"min_delay"`
	MinTimeSinceLastHit string                 `yaml:"min_time_since_last_hit"`
	Repeated            int                    `yaml:"repeated"`
}

type BehavioralChainYAML struct {
	Name          string        `yaml:"name"`
	Action        string        `yaml:"action"`
	BlockDuration string        `yaml:"block_duration"`
	MatchKey      string        `yaml:"match_key"`
	OnMatch       string        `yaml:"on_match"`
	Steps         []StepDefYAML `yaml:"steps"`
}

// --- RUNTIME DATA STRUCTURES ---

type LogEntry struct {
	Timestamp  time.Time // Actual time of the request (parsed from log, not time.Now()).
	IPInfo     utils.IPInfo
	Method     string
	Path       string
	Protocol   string
	Referrer   string
	StatusCode int
	Size       int
	UserAgent  string
	VHost      string
}

// Actor is a comparable struct used as the key for the ActivityStore map. It represents
// the unique entity being tracked (e.g., an IP address or an IP+UserAgent combination).
type Actor struct {
	IPInfo utils.IPInfo
	UA     string // UserAgent. Empty string if tracking is IP-only.
}

// String provides a clean, readable representation of the Actor for logging.
func (a Actor) String() string {
	// Use a separator that is unlikely to appear in a User-Agent string.
	if a.UA != "" {
		return fmt.Sprintf("%s | %s", a.IPInfo.Address, a.UA)
	}
	return a.IPInfo.Address
}

// SkipInfo holds structured information about why an actor was skipped.
type SkipInfo struct {
	Type   utils.SkipType
	Source string // The name of the good_actor rule or the blocking chain.
}

// StepState holds the progress of an actor within a single behavioral chain.
type StepState struct {
	CurrentStep   int
	LastMatchTime time.Time
}

// StepDef holds the compiled definition of a single step in a behavioral chain.
type StepDef struct {
	Order    int
	Matchers []struct {
		Matcher   fieldMatcher
		FieldName string
	} // Changed: Now stores matcher and its associated field name.
	MaxDelayDuration    time.Duration
	MinDelayDuration    time.Duration
	MinTimeSinceLastHit time.Duration
}

// BehavioralChain holds the compiled definition of a single behavioral chain.
type BehavioralChain struct {
	Name                     string
	Action                   string
	BlockDuration            time.Duration
	BlockDurationStr         string        // The original string representation of the duration (e.g., "1w")
	UsesDefaultBlockDuration bool          // True if the chain is using the global default_block_duration.
	MatchKey                 string        // (ip, ipv4, ipv6, ip_ua, ipv4_ua, ipv6_ua)
	OnMatch                  string        // "stop" to halt processing of other chains on match.
	StepsYAML                []StepDefYAML // Store original YAML for accurate comparison
	Steps                    []StepDef
	MetricsHitsCounter       *atomic.Int64 // Counter for hits on this specific chain.
	MetricsResetCounter      *atomic.Int64 // Counter for resets of this specific chain.
	MetricsCounter           *atomic.Int64 // Counter for this specific chain.
	FieldMatchCounts         *sync.Map     // Counter for field matches within this chain (key: fieldName, value: *atomic.Int64).
}

// GoodActorDef represents a single compiled definition from the good_actors config.

type GoodActorDef struct {
	Name string

	IPMatchers []fieldMatcher // A list of matchers for the IP field (OR logic within the list)

	UAMatchers []fieldMatcher // A list of matchers for the UserAgent field (OR logic within the list)

}
