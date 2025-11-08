package main

import (
	"net"
	"sync"
	"time"
)

// --- EXTERNAL INTERFACES ---

// IPVersion is used internally to track whether an IP is v4 or v6.
type IPVersion byte

// Blocker defines the interface for external IP blocking services (e.g., HAProxy).
type Blocker interface {
	Block(ipInfo IPInfo, duration time.Duration) error
	Unblock(ipInfo IPInfo) error
}

// --- DEPENDENCY CONTAINER ---

// Processor holds all necessary dependencies and state for log processing,
// making it easy to mock/stub external calls and manage state in tests.
type Processor struct {
	ActivityStore        map[TrackingKey]*BotActivity
	ActivityMutex        *sync.RWMutex
	Chains               []BehavioralChain
	ChainMutex           *sync.RWMutex
	DryRun               bool
	LogFunc              func(level LogLevel, tag string, format string, args ...interface{})
	IsWhitelistedFunc    func(ipInfo IPInfo) bool
	ProcessLogLine       func(line string, lineNumber int)
	Blocker              Blocker
	CommandExecutor      func(p *Processor, addr, ip, command string) error // The function that executes the backend command
	Config               *AppConfig
	testForceCheckSignal chan struct{} // Test-only: Signals the watcher to run a check now.
	testReloadDoneSignal chan struct{} // Test-only: The watcher signals when a reload cycle is complete.
}

// AppConfig holds all the configuration state that can be reloaded from YAML.
type AppConfig struct {
	BlockTableNameFallback      string
	CleanupInterval             time.Duration
	DurationToTableName         map[time.Duration]string
	FileDependencies            []string // List of file paths used in `file:` matchers.
	HAProxyAddresses            []string
	HAProxyDialTimeout          time.Duration
	HAProxyMaxRetries           int
	HAProxyRetryDelay           time.Duration
	IdleTimeout                 time.Duration
	LastModTime                 time.Time
	MaxTimeSinceLastHit         time.Duration // The longest min_time_since_last_hit duration across all chains.
	OutOfOrderTolerance         time.Duration // Max duration an out-of-order log entry will be processed.
	PollingInterval             time.Duration
	WhitelistNets               []*net.IPNet
	testOverridePollingInterval time.Duration // Unexported field for test-only overrides.
}

// LoadedConfig encapsulates all configuration data loaded from the YAML file.
type LoadedConfig struct {
	BlockTableNameFallback string
	Chains                 []BehavioralChain
	CleanupInterval        time.Duration
	DurationToTableName    map[time.Duration]string
	FileDependencies       []string
	HAProxyAddresses       []string
	HAProxyDialTimeout     time.Duration
	HAProxyMaxRetries      int
	HAProxyRetryDelay      time.Duration
	IdleTimeout            time.Duration
	LogLevel               string
	MaxTimeSinceLastHit    time.Duration
	OutOfOrderTolerance    time.Duration
	PollingInterval        time.Duration
	WhitelistNets          []*net.IPNet
}

// --- YAML DATA STRUCTURES ---

type ChainConfig struct {
	Chains               []BehavioralChainYAML `yaml:"chains"`
	CleanupInterval      string                `yaml:"cleanup_interval"`
	DefaultBlockDuration string                `yaml:"default_block_duration"`
	DurationTables       map[string]string     `yaml:"duration_tables"`
	HAProxyAddresses     []string              `yaml:"haproxy_addresses"`
	HAProxyDialTimeout   string                `yaml:"haproxy_dial_timeout"`
	HAProxyMaxRetries    int                   `yaml:"haproxy_max_retries"`
	HAProxyRetryDelay    string                `yaml:"haproxy_retry_delay"`
	IdleTimeout          string                `yaml:"idle_timeout"`
	LogLevel             string                `yaml:"log_level"`
	OutOfOrderTolerance  string                `yaml:"out_of_order_tolerance"`
	PollingInterval      string                `yaml:"poll_interval"`
	Version              string                `yaml:"version"`
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
	Action        string        `yaml:"action"`
	BlockDuration string        `yaml:"block_duration"`
	MatchKey      string        `yaml:"match_key"`
	Name          string        `yaml:"name"`
	Steps         []StepDefYAML `yaml:"steps"`
}

// --- RUNTIME DATA STRUCTURES ---

type LogEntry struct {
	Timestamp  time.Time // Actual time of the request (parsed from log, not time.Now()).
	IPInfo     IPInfo
	Path       string
	Method     string
	Protocol   string
	UserAgent  string
	Referrer   string
	StatusCode int
}

type StepDef struct {
	Order               int
	MaxDelayDuration    time.Duration
	MinDelayDuration    time.Duration
	MinTimeSinceLastHit time.Duration
	Matchers            []fieldMatcher // Pre-compiled matcher functions for performance.
}

type BehavioralChain struct {
	Name          string
	Steps         []StepDef
	Action        string
	BlockDuration time.Duration
	MatchKey      string // (ip, ipv4, ipv6, ip_ua, ipv4_ua, ipv6_ua)
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
	LastRequestTime time.Time // Time of the IP's PREVIOUS overall request.
	ChainProgress   map[string]StepState
	IsBlocked       bool      // Flag to skip chain checks if this key is blocked.
	BlockedUntil    time.Time // Time when the block expires.
}
