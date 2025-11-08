package main

import (
	"net"
	"os"
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
	ActivityMutex     *sync.RWMutex
	ActivityStore     map[TrackingKey]*BotActivity
	Blocker           Blocker
	ChainMutex        *sync.RWMutex
	Chains            []BehavioralChain
	CommandExecutor   func(p *Processor, addr, ip, command string) error // The function that executes the backend command
	Config            *AppConfig
	DryRun            bool
	IsWhitelistedFunc func(ipInfo IPInfo) bool
	CheckChainsFunc   func(entry *LogEntry)
	signalCh          chan os.Signal
	LogFunc           func(level LogLevel, tag string, format string, v ...interface{})
	ProcessLogLine    func(line string, lineNumber int)
}

// AppConfig holds all the configuration state that can be reloaded from YAML.
type AppConfig struct {
	BlockTableNameFallback string
	CleanupInterval        time.Duration
	DurationToTableName    map[time.Duration]string
	EOFPollingDelay        time.Duration
	FileDependencies       []string // List of file paths used in `file:` matchers.
	HAProxyAddresses       []string
	HAProxyDialTimeout     time.Duration
	HAProxyMaxRetries      int
	HAProxyRetryDelay      time.Duration
	IdleTimeout            time.Duration
	LastModTime            time.Time
	MaxTimeSinceLastHit    time.Duration // The longest min_time_since_last_hit duration across all chains.
	OutOfOrderTolerance    time.Duration // Max duration an out-of-order log entry will be processed.
	PollingInterval        time.Duration
	WhitelistNets          []*net.IPNet
}

// LoadedConfig encapsulates all configuration data loaded from the YAML file.
type LoadedConfig struct {
	BlockTableNameFallback string
	Chains                 []BehavioralChain
	CleanupInterval        time.Duration
	DurationToTableName    map[time.Duration]string
	EOFPollingDelay        time.Duration
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
	LogLevel             string                `yaml:"log_level"`
	OutOfOrderTolerance  string                `yaml:"out_of_order_tolerance"`
	PollingInterval      string                `yaml:"poll_interval"`
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
	Name          string
	Action        string
	BlockDuration time.Duration
	MatchKey      string // (ip, ipv4, ipv6, ip_ua, ipv4_ua, ipv6_ua)
	Steps         []StepDef
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
