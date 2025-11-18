package app

import (
	"bot-detector/internal/blocker"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	metrics "bot-detector/internal/metrics"
	"bot-detector/internal/persistence"
	"bot-detector/internal/store"
	"bot-detector/internal/types"
	"os"
	"regexp"
	"sync"
	"time"
)

// Type aliases for types moved to internal/types package
type (
	FieldType = types.FieldType
	LogEntry  = types.LogEntry
)

// Re-export constants from types package
const (
	StringField      = types.StringField
	IntField         = types.IntField
	UnsupportedField = types.UnsupportedField
)

// Re-export variables from types package
var FieldNameCanonicalMap = types.FieldNameCanonicalMap

// Re-export functions from types package
var (
	GetMatchValue       = types.GetMatchValue
	GetMatchValueIfType = types.GetMatchValueIfType
)

// TestSignals holds channels used exclusively for test synchronization.
// This struct is nil in production.
type TestSignals struct {
	CleanupDoneSignal chan struct{}
	EOFCheckSignal    chan struct{}
	ReloadDoneSignal  chan struct{}
	ForceCheckSignal  chan struct{}
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
	Chains        []config.BehavioralChain
	Config        *config.AppConfig
	DryRun        bool
	EnableMetrics bool

	EntryBuffer          []*LogEntry    // Buffer for holding out-of-order entries.
	OooBufferFlushSignal chan struct{}  // Signal to the entryBufferWorker to flush the OOO buffer immediately.
	LogRegex             *regexp.Regexp // The currently active log parsing regex.
	CheckChainsFunc      func(entry *LogEntry)
	SignalCh             chan os.Signal
	LogFunc              func(level logging.LogLevel, tag string, format string, v ...interface{})
	ProcessLogLine       func(line string)
	NowFunc              func() time.Time // Mockable time function.
	SignalOooBufferFlush func()
	TestSignals          *TestSignals // Test-only signals for synchronization.
	ConfigPath           string
	LogPath              string `test:"-"`
	ReloadOn             string
	TopActorsPerChain    map[string]map[string]*store.ActorStats // Dry-run only: tracks top actors per chain.
	HTTPServer           string
	TopN                 int // For dry-run stats: show top N actors.
	StartTime            time.Time
	// Persistence fields
	PersistenceEnabled bool
	StateDir           string
	CompactionInterval time.Duration
	PersistenceMutex   sync.Mutex
	PersistenceWg      sync.WaitGroup
	JournalHandle      *os.File
	ActiveBlocks       map[string]persistence.ActiveBlockInfo
	// ConfigReloaded is a flag to indicate if the configuration has been reloaded at least once.
	ConfigReloaded bool
	// ExitOnEOF is a flag to indicate if the tailer should exit when it reaches the end of the file.
	ExitOnEOF bool
	// Cluster fields
	NodeRole          string // "leader" or "follower"
	NodeName          string // Node name from cluster config (empty if not configured)
	NodeAddress       string // Node address from cluster config (empty if not configured)
	NodeLeaderAddress string // Leader address (only set for followers)
}

// GetTimestampFormat returns the timestamp format from the config.
// This method allows Processor to satisfy the parser.Provider interface.
func (p *Processor) GetTimestampFormat() string {
	p.ConfigMutex.RLock()
	defer p.ConfigMutex.RUnlock()
	return p.Config.Parser.TimestampFormat
}

// GetLogRegex returns the currently active log parsing regex.
// This method allows Processor to satisfy the parser.Provider interface.
func (p *Processor) GetLogRegex() *regexp.Regexp {
	p.ConfigMutex.RLock()
	defer p.ConfigMutex.RUnlock()
	return p.LogRegex
}

// GetMarshalledConfig reads the raw configuration file from disk.
func (p *Processor) GetMarshalledConfig() ([]byte, time.Time, error) {
	p.ConfigMutex.RLock()
	defer p.ConfigMutex.RUnlock()
	return p.Config.YAMLContent, p.Config.LastModTime, nil
}

// AppConfig holds all the configuration state that can be reloaded from YAML.

// Config types moved to internal/config/types.go
