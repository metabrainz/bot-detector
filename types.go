package main

import (
	"bot-detector/internal/blocker"
	"bot-detector/internal/logging"
	metrics "bot-detector/internal/metrics"
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
	Config        *AppConfig
	DryRun        bool

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
	configReloaded       bool
}

// AppConfig holds all the configuration state that can be reloaded from YAML.
type AppConfig struct {
	GoodActors               []GoodActorDef                    `config:"compare"`
	BlockTableNameFallback   string                            `config:"compare"` // This is derived, but comparing is harmless and simple.
	CleanupInterval          time.Duration                     `config:"compare" summary:"cleanup_interval"`
	DurationToTableName      map[time.Duration]string          `config:"compare" summary:"duration_tables"`
	DefaultBlockDuration     time.Duration                     `config:"compare" summary:"default_block_duration"`
	EOFPollingDelay          time.Duration                     `config:"compare" summary:"eof_polling_delay"`
	FileDependencies         map[string]*FileDependency        // Map of file paths to their dependency status.
	BlockerAddresses         []string                          `config:"compare" summary:"blocker_addresses"`
	BlockerDialTimeout       time.Duration                     `config:"compare" summary:"blocker_dial_timeout"`
	BlockerMaxRetries        int                               `config:"compare" summary:"blocker_max_retries"`
	BlockerRetryDelay        time.Duration                     `config:"compare" summary:"blocker_retry_delay"`
	BlockerCommandQueueSize  int                               `config:"compare" summary:"blocker_command_queue_size"`
	BlockerCommandsPerSecond int                               `config:"compare" summary:"blocker_commands_per_second"`
	IdleTimeout              time.Duration                     `config:"compare" summary:"idle_timeout"`
	LineEnding               string                            `config:"compare" summary:"line_ending"`
	LastModTime              time.Time                         // Not compared
	MaxTimeSinceLastHit      time.Duration                     `config:"compare" summary:"max_time_since_last_hit"`
	OutOfOrderTolerance      time.Duration                     `config:"compare" summary:"out_of_order_tolerance"`
	PollingInterval          time.Duration                     `config:"compare" summary:"polling_interval"`
	TimestampFormat          string                            `config:"compare"`
	UnblockOnGoodActor       bool                              `config:"compare"`
	UnblockCooldown          time.Duration                     `config:"compare"`
	LogFormatRegex           string                            `config:"compare"`
	StatFunc                 func(string) (os.FileInfo, error) // Mockable
	FileOpener               fileOpener                        // Mockable
}

// Clone creates a deep copy of the AppConfig. This is crucial for safely comparing
// configurations during a reload without causing race conditions.
func (c *AppConfig) Clone() AppConfig {
	if c == nil {
		return AppConfig{}
	}

	// Create a new config and copy value types.
	clone := *c

	// Deep copy slices and maps.
	if c.GoodActors != nil {
		clone.GoodActors = make([]GoodActorDef, len(c.GoodActors)) // GoodActorDef contains slices, so we need to copy them too.
		copy(clone.GoodActors, c.GoodActors)
		for i, def := range c.GoodActors {
			if def.IPMatchers != nil {
				clone.GoodActors[i].IPMatchers = make([]fieldMatcher, len(def.IPMatchers))
				copy(clone.GoodActors[i].IPMatchers, def.IPMatchers)
			}
			if def.UAMatchers != nil {
				clone.GoodActors[i].UAMatchers = make([]fieldMatcher, len(def.UAMatchers))
				copy(clone.GoodActors[i].UAMatchers, def.UAMatchers)
			}
		}
	}

	if c.DurationToTableName != nil {
		clone.DurationToTableName = make(map[time.Duration]string, len(c.DurationToTableName))
		for k, v := range c.DurationToTableName {
			clone.DurationToTableName[k] = v
		}
	}

	if c.FileDependencies != nil {
		newFileDependencies := make(map[string]*FileDependency, len(c.FileDependencies))
		for k, v := range c.FileDependencies {
			newFileDependencies[k] = v.Clone() // Deep copy the FileDependency object
		}
		clone.FileDependencies = newFileDependencies
	}
	// Other slice types are just strings, which are immutable, so a shallow copy is fine.
	return clone
}

// LoadedConfig encapsulates all configuration data loaded from the YAML file.
type LoadedConfig struct {
	GoodActors               []GoodActorDef           `config:"compare"`
	BlockTableNameFallback   string                   `config:"compare"`
	Chains                   []BehavioralChain        // Not compared here
	CleanupInterval          time.Duration            `config:"compare"`
	DefaultBlockDuration     time.Duration            `config:"compare"`
	DurationToTableName      map[time.Duration]string `config:"compare"`
	EOFPollingDelay          time.Duration            `config:"compare"`
	FileDependencies         map[string]*FileDependency
	BlockerAddresses         []string       `config:"compare"`
	BlockerDialTimeout       time.Duration  `config:"compare"`
	BlockerMaxRetries        int            `config:"compare"`
	BlockerRetryDelay        time.Duration  `config:"compare"`
	BlockerCommandQueueSize  int            `config:"compare"`
	BlockerCommandsPerSecond int            `config:"compare"`
	IdleTimeout              time.Duration  `config:"compare"`
	LogLevel                 string         `config:"compare"`
	LineEnding               string         `config:"compare"`
	LogFormatRegex           *regexp.Regexp // Not compared here
	MaxTimeSinceLastHit      time.Duration  `config:"compare"`
	OutOfOrderTolerance      time.Duration  `config:"compare"`
	PollingInterval          time.Duration  `config:"compare"`
	TimestampFormat          string         `config:"compare"`
	UnblockOnGoodActor       bool           `config:"compare"`
	UnblockCooldown          time.Duration  `config:"compare"`
	StatFunc                 func(string) (os.FileInfo, error)
}

// --- YAML DATA STRUCTURES ---

type ChainConfig struct {
	GoodActors               []map[string]interface{} `yaml:"good_actors"`
	Version                  string                   `yaml:"version"`
	Chains                   []BehavioralChainYAML    `yaml:"chains"`
	CleanupInterval          string                   `yaml:"cleanup_interval"`
	DefaultBlockDuration     string                   `yaml:"default_block_duration"`
	DurationTables           map[string]string        `yaml:"duration_tables"`
	EOFPollingDelay          string                   `yaml:"eof_polling_delay"`
	BlockerAddresses         []string                 `yaml:"blocker_addresses"`
	BlockerDialTimeout       string                   `yaml:"blocker_dial_timeout"`
	BlockerMaxRetries        int                      `yaml:"blocker_max_retries"`
	BlockerRetryDelay        string                   `yaml:"blocker_retry_delay"`
	BlockerCommandQueueSize  int                      `yaml:"blocker_command_queue_size"`
	BlockerCommandsPerSecond int                      `yaml:"blocker_commands_per_second"`
	IdleTimeout              string                   `yaml:"idle_timeout"`
	LineEnding               string                   `yaml:"line_ending"`
	LogLevel                 string                   `yaml:"log_level"`
	LogFormatRegex           string                   `yaml:"log_format_regex"`
	OutOfOrderTolerance      string                   `yaml:"out_of_order_tolerance"`
	PollingInterval          string                   `yaml:"polling_interval"`
	TimestampFormat          string                   `yaml:"timestamp_format"`
	UnblockOnGoodActor       bool                     `yaml:"unblock_on_good_actor"`
	UnblockCooldown          string                   `yaml:"unblock_cooldown"`
}

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
	Order               int
	Matchers            []fieldMatcher // Pre-compiled matcher functions for performance.
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
}

// GoodActorDef represents a single compiled definition from the good_actors config.
type GoodActorDef struct {
	Name       string
	IPMatchers []fieldMatcher // A list of matchers for the IP field (OR logic within the list)
	UAMatchers []fieldMatcher // A list of matchers for the UserAgent field (OR logic within the list)
}
