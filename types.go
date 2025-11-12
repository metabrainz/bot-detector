package main

import (
	"bot-detector/internal/logging"
	metrics "bot-detector/internal/metrics"
	"fmt"

	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

// --- EXTERNAL INTERFACES ---

// IPVersion is used internally to track whether an IP is v4 or v6.
type IPVersion byte

// FieldType indicates the native type of a field from a LogEntry.
type FieldType int

const (
	StringField FieldType = iota
	IntField
	UnsupportedField
)

// SkipType defines the reason an actor's log entry was skipped.
type SkipType byte

const (
	// SkipTypeNone is the zero value, indicating no skip.
	SkipTypeNone SkipType = iota
	SkipTypeGoodActor
	SkipTypeBlocked
)

// TestSignals holds channels used exclusively for test synchronization.
// This struct is nil in production.
type TestSignals struct {
	CleanupDoneSignal chan struct{}
	EOFCheckSignal    chan struct{}
	ReloadDoneSignal  chan struct{}
	ForceCheckSignal  chan struct{}
}

// Blocker defines the interface for external IP blocking services (e.g., HAProxy).
type Blocker interface {
	Block(ipInfo IPInfo, duration time.Duration) error
	Unblock(ipInfo IPInfo) error
}

// --- DEPENDENCY CONTAINER ---

// Processor holds all necessary dependencies and state for log processing,
// making it easy to mock/stub external calls and manage state in tests.
type Processor struct {
	ActivityMutex   *sync.RWMutex
	ActivityStore   map[Actor]*ActorActivity
	Blocker         Blocker
	ConfigMutex     *sync.RWMutex
	Metrics         *metrics.Metrics
	Chains          []BehavioralChain
	CommandExecutor func(p *Processor, addr, ip, command string) error // The function that executes the backend command
	Config          *AppConfig
	DryRun          bool

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
	LogPath              string
	ReloadOnSignal       string
	TopActorsPerChain    map[string]map[string]*ActorStats // Dry-run only: tracks top actors per chain.
	TopN                 int                               // For dry-run stats: show top N actors.
	startTime            time.Time
}

// AppConfig holds all the configuration state that can be reloaded from YAML.
type AppConfig struct {
	GoodActors               []GoodActorDef                    `config:"compare"`
	BlockTableNameFallback   string                            `config:"compare"` // This is derived, but comparing is harmless and simple.
	CleanupInterval          time.Duration                     `config:"compare" summary:"cleanup_interval"`
	DurationToTableName      map[time.Duration]string          `config:"compare" summary:"duration_tables"`
	DefaultBlockDuration     time.Duration                     `config:"compare" summary:"default_block_duration"`
	EOFPollingDelay          time.Duration                     `config:"compare" summary:"eof_polling_delay"`
	FileDependencies         []string                          // List of file paths used in `file:` matchers.
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
	PollingInterval          time.Duration                     `config:"compare" summary:"poll_interval"`
	TimestampFormat          string                            `config:"compare"`
	LogFormatRegex           string                            `config:"compare"`
	HTTPListenAddr           string                            `config:"compare" summary:"http_listen_addr"`
	StatFunc                 func(string) (os.FileInfo, error) // Mockable

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
	FileDependencies         []string                 // Not compared
	BlockerAddresses         []string                 `config:"compare"`
	BlockerDialTimeout       time.Duration            `config:"compare"`
	BlockerMaxRetries        int                      `config:"compare"`
	BlockerRetryDelay        time.Duration            `config:"compare"`
	BlockerCommandQueueSize  int                      `config:"compare"`
	BlockerCommandsPerSecond int                      `config:"compare"`
	IdleTimeout              time.Duration            `config:"compare"`
	LogLevel                 string                   `config:"compare"`
	LineEnding               string                   `config:"compare"`
	LogFormatRegex           *regexp.Regexp           // Not compared here
	MaxTimeSinceLastHit      time.Duration            `config:"compare"`
	OutOfOrderTolerance      time.Duration            `config:"compare"`
	PollingInterval          time.Duration            `config:"compare"`
	TimestampFormat          string                   `config:"compare"`
	HTTPListenAddr           string                   `config:"compare"`
	StatFunc                 func(string) (os.FileInfo, error)
}

// --- YAML DATA STRUCTURES ---

type ChainConfig struct {
	GoodActors               map[string]map[string]interface{} `yaml:"good_actors"`
	Version                  string                            `yaml:"version"`
	Chains                   []BehavioralChainYAML             `yaml:"chains"`
	CleanupInterval          string                            `yaml:"cleanup_interval"`
	DefaultBlockDuration     string                            `yaml:"default_block_duration"`
	DurationTables           map[string]string                 `yaml:"duration_tables"`
	EOFPollingDelay          string                            `yaml:"eof_polling_delay"`
	BlockerAddresses         []string                          `yaml:"blocker_addresses"`
	BlockerDialTimeout       string                            `yaml:"blocker_dial_timeout"`
	BlockerMaxRetries        int                               `yaml:"blocker_max_retries"`
	BlockerRetryDelay        string                            `yaml:"blocker_retry_delay"`
	BlockerCommandQueueSize  int                               `yaml:"blocker_command_queue_size"`
	BlockerCommandsPerSecond int                               `yaml:"blocker_commands_per_second"`
	IdleTimeout              string                            `yaml:"idle_timeout"`
	HTTPListenAddr           string                            `yaml:"http_listen_addr"`
	LineEnding               string                            `yaml:"line_ending"`
	LogLevel                 string                            `yaml:"log_level"`
	LogFormatRegex           string                            `yaml:"log_format_regex"`
	OutOfOrderTolerance      string                            `yaml:"out_of_order_tolerance"`
	PollingInterval          string                            `yaml:"poll_interval"`
	TimestampFormat          string                            `yaml:"timestamp_format"`
}

type StepDefYAML struct {
	Order               int
	FieldMatches        map[string]interface{} `yaml:"field_matches"`
	MaxDelay            string                 `yaml:"max_delay"`
	MinDelay            string                 `yaml:"min_delay"`
	MinTimeSinceLastHit string                 `yaml:"min_time_since_last_hit"`
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
	IPInfo     IPInfo
	Method     string
	Path       string
	Protocol   string
	Referrer   string
	StatusCode int
	Size       int
	UserAgent  string
	VHost      string
}

type StepDef struct {
	Order               int
	Matchers            []fieldMatcher // Pre-compiled matcher functions for performance.
	MaxDelayDuration    time.Duration
	MinDelayDuration    time.Duration
	MinTimeSinceLastHit time.Duration
}

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

// SkipInfo holds structured information about why an actor was skipped.
type SkipInfo struct {
	Type   SkipType
	Source string // The name of the good_actor rule or the blocking chain.
}

// ActorStats holds hit and completion counts for a specific actor in a chain.
type ActorStats struct {
	Hits        int64
	Completions int64
	Resets      int64
}

type StepState struct {
	CurrentStep   int
	LastMatchTime time.Time
}

// Actor is a comparable struct used as the key for the ActivityStore map. It represents
// the unique entity being tracked (e.g., an IP address or an IP+UserAgent combination).
type Actor struct {
	IPInfo IPInfo
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

// ActorActivity tracks state for a single actor (IP address or IP+UA combination) across all chains.
type ActorActivity struct {
	LastRequestTime time.Time // Time of the actor's most recent request.
	BlockedUntil    time.Time // Time when the block expires.
	ChainProgress   map[string]StepState
	IsBlocked       bool // Flag to skip chain checks if this actor is blocked.
	SkipInfo        SkipInfo
}
