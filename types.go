package main

import (
	"bot-detector/internal/logging"
	"net"
	"os"
	"regexp"
	"sync"
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
	ActivityMutex     *sync.RWMutex
	ActivityStore     map[TrackingKey]*BotActivity
	Blocker           Blocker
	ConfigMutex       *sync.RWMutex
	Chains            []BehavioralChain
	CommandExecutor   func(p *Processor, addr, ip, command string) error // The function that executes the backend command
	Config            *AppConfig
	DryRun            bool
	IsWhitelistedFunc func(ipInfo IPInfo) bool
	EntryBuffer       []*LogEntry    // Buffer for holding out-of-order entries.
	LogRegex          *regexp.Regexp // The currently active log parsing regex.
	CheckChainsFunc   func(entry *LogEntry)
	signalCh          chan os.Signal
	LogFunc           func(level logging.LogLevel, tag string, format string, v ...interface{})
	ProcessLogLine    func(line string)
	NowFunc           func() time.Time // Mockable time function.
	TestSignals       *TestSignals     // Test-only signals for synchronization.
}

// AppConfig holds all the configuration state that can be reloaded from YAML.
type AppConfig struct {
	BlockTableNameFallback string                            `config:"compare"` // This is derived, but comparing is harmless and simple.
	CleanupInterval        time.Duration                     `config:"compare" summary:"cleanup_interval"`
	DurationToTableName    map[time.Duration]string          `config:"compare" summary:"duration_tables"`
	DefaultBlockDuration   time.Duration                     `config:"compare" summary:"default_block_duration"`
	EOFPollingDelay        time.Duration                     `config:"compare" summary:"eof_polling_delay"`
	FileDependencies       []string                          // List of file paths used in `file:` matchers.
	HAProxyAddresses       []string                          `config:"compare" summary:"haproxy_addresses"`
	HAProxyDialTimeout     time.Duration                     `config:"compare" summary:"haproxy_dial_timeout"`
	HAProxyMaxRetries      int                               `config:"compare" summary:"haproxy_max_retries"`
	HAProxyRetryDelay      time.Duration                     `config:"compare" summary:"haproxy_retry_delay"`
	IdleTimeout            time.Duration                     `config:"compare" summary:"idle_timeout"`
	LineEnding             string                            `config:"compare" summary:"line_ending"`
	LastModTime            time.Time                         // Not compared
	MaxTimeSinceLastHit    time.Duration                     `config:"compare" summary:"max_time_since_last_hit"`
	OutOfOrderTolerance    time.Duration                     `config:"compare" summary:"out_of_order_tolerance"`
	PollingInterval        time.Duration                     `config:"compare" summary:"poll_interval"`
	TimestampFormat        string                            `config:"compare"`
	StatFunc               func(string) (os.FileInfo, error) // Mockable
	WhitelistNets          []*net.IPNet                      `config:"compare"`
}

// LoadedConfig encapsulates all configuration data loaded from the YAML file.
type LoadedConfig struct {
	BlockTableNameFallback string                   `config:"compare"`
	Chains                 []BehavioralChain        // Not compared here
	CleanupInterval        time.Duration            `config:"compare"`
	DefaultBlockDuration   time.Duration            `config:"compare"`
	DurationToTableName    map[time.Duration]string `config:"compare"`
	EOFPollingDelay        time.Duration            `config:"compare"`
	FileDependencies       []string                 // Not compared
	HAProxyAddresses       []string                 `config:"compare"`
	HAProxyDialTimeout     time.Duration            `config:"compare"`
	HAProxyMaxRetries      int                      `config:"compare"`
	HAProxyRetryDelay      time.Duration            `config:"compare"`
	IdleTimeout            time.Duration            `config:"compare"`
	LogLevel               string                   `config:"compare"`
	LineEnding             string                   `config:"compare"`
	LogFormatRegex         *regexp.Regexp           // Not compared here
	MaxTimeSinceLastHit    time.Duration            `config:"compare"`
	OutOfOrderTolerance    time.Duration            `config:"compare"`
	PollingInterval        time.Duration            `config:"compare"`
	TimestampFormat        string                   `config:"compare"`
	StatFunc               func(string) (os.FileInfo, error)
	WhitelistNets          []*net.IPNet `config:"compare"`
}

// --- YAML DATA STRUCTURES ---

type ChainConfig struct {
	Version              string                `yaml:"version"`
	Chains               []BehavioralChainYAML `yaml:"chains"`
	CleanupInterval      string                `yaml:"cleanup_interval"`
	DefaultBlockDuration string                `yaml:"default_block_duration"`
	DurationTables       map[string]string     `yaml:"duration_tables"`
	EOFPollingDelay      string                `yaml:"eof_polling_delay"`
	HAProxyAddresses     []string              `yaml:"haproxy_addresses"`
	HAProxyDialTimeout   string                `yaml:"haproxy_dial_timeout"`
	HAProxyMaxRetries    int                   `yaml:"haproxy_max_retries"`
	HAProxyRetryDelay    string                `yaml:"haproxy_retry_delay"`
	IdleTimeout          string                `yaml:"idle_timeout"`
	LineEnding           string                `yaml:"line_ending"`
	LogLevel             string                `yaml:"log_level"`
	LogFormatRegex       string                `yaml:"log_format_regex"`
	OutOfOrderTolerance  string                `yaml:"out_of_order_tolerance"`
	PollingInterval      string                `yaml:"poll_interval"`
	TimestampFormat      string                `yaml:"timestamp_format"`
	WhitelistCIDRs       []string              `yaml:"whitelist_cidrs"`
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
	UserAgent  string
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
	UsesDefaultBlockDuration bool          // True if the chain is using the global default_block_duration.
	MatchKey                 string        // (ip, ipv4, ipv6, ip_ua, ipv4_ua, ipv6_ua)
	StepsYAML                []StepDefYAML // Store original YAML for accurate comparison
	Steps                    []StepDef
}

type StepState struct {
	CurrentStep   int
	LastMatchTime time.Time
}

// TrackingKey is a comparable struct used as the key for the ActivityStore map.
type TrackingKey struct {
	IPInfo IPInfo
	UA     string // UserAgent. Empty string if tracking is IP-only.
}

// BotActivity tracks state for a single IP address (or IP+UA combination) across all chains.
type BotActivity struct {
	LastRequestTime time.Time // Time of the IP's most recent request.
	BlockedUntil    time.Time // Time when the block expires.
	ChainProgress   map[string]StepState
	IsBlocked       bool // Flag to skip chain checks if this key is blocked.
}
