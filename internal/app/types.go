package app

import (
	"bot-detector/internal/blocker"
	"bot-detector/internal/config"
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

// LogEntry represents a parsed log entry with all its fields.
type LogEntry struct {
	Timestamp  time.Time
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

// GetMatchValue returns the value and type of a field from a LogEntry.
func GetMatchValue(fieldName string, entry *LogEntry) (interface{}, FieldType, error) {
	// If entry is nil, this is a compile-time check for the field's type.
	if entry == nil {
		entry = &LogEntry{} // Use a zero-value entry to get the type.
	}

	switch fieldName {
	case "IP":
		return entry.IPInfo.Address, StringField, nil
	case "Path":
		return entry.Path, StringField, nil
	case "Method":
		return entry.Method, StringField, nil
	case "Protocol":
		return entry.Protocol, StringField, nil
	case "UserAgent":
		return entry.UserAgent, StringField, nil
	case "Referrer":
		return entry.Referrer, StringField, nil
	case "StatusCode":
		return entry.StatusCode, IntField, nil
	case "Size":
		return entry.Size, IntField, nil
	case "VHost":
		return entry.VHost, StringField, nil
	default:
		return nil, UnsupportedField, fmt.Errorf("unknown field: '%s'", fieldName)
	}
}

// GetMatchValueIfType retrieves a field's value only if it matches the expected type.

func GetMatchValueIfType(fieldName string, entry *LogEntry, expectedType FieldType) interface{} {
	value, actualType, err := GetMatchValue(fieldName, entry) //nolint:errcheck
	if err != nil || actualType != expectedType {
		return nil
	}
	return value
}
