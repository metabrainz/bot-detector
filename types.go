package main

import (
	"net"
	"regexp"
	"sync"
	"time"
)

// --- EXTERNAL INTERFACES ---

// IPVersion is used internally to track whether an IP is v4 or v6.
type IPVersion byte

// Blocker defines the interface for external IP blocking services (e.g., HAProxy).
type Blocker interface {
	Block(ipInfo IPInfo, duration time.Duration) error
}

// GlobalBlocker is a concrete implementation of Blocker that uses the original
// global BlockIP function, ensuring backward compatibility.
// NOTE: BlockIP must be declared globally in the package scope for this to compile.
type GlobalBlocker struct{}

// Block calls the original global BlockIP function.
func (h *GlobalBlocker) Block(ipInfo IPInfo, duration time.Duration) error {
	return P.BlockIP(ipInfo, duration)
}

// --- DEPENDENCY CONTAINER ---

// Processor holds all necessary dependencies and state for log processing,
// making it easy to mock/stub external calls and manage state in tests.
type Processor struct {
	ActivityStore     map[TrackingKey]*BotActivity
	ActivityMutex     *sync.RWMutex
	Chains            []BehavioralChain
	ChainMutex        *sync.RWMutex
	DryRun            bool
	LogFunc           func(level LogLevel, tag string, format string, args ...interface{})
	IsWhitelistedFunc func(ipInfo IPInfo) bool
	Blocker           Blocker
	Config            *AppConfig
}

// AppConfig holds all the configuration state that can be reloaded from YAML.
type AppConfig struct {
	WhitelistNets               []*net.IPNet
	HAProxyAddresses            []string
	DurationToTableName         map[time.Duration]string
	BlockTableNameFallback      string
	LastModTime                 time.Time
	PollingInterval             time.Duration
	IdleTimeout                 time.Duration
	CleanupInterval             time.Duration
	MaxTimeSinceLastHit         time.Duration // The longest min_time_since_last_hit duration across all chains.
	OutOfOrderTolerance         time.Duration // Max duration an out-of-order log entry will be processed.
	testOverridePollingInterval time.Duration // Unexported field for test-only overrides.
}

// LoadedConfig encapsulates all configuration data loaded from the YAML file.
type LoadedConfig struct {
	Chains                 []BehavioralChain
	WhitelistNets          []*net.IPNet
	HAProxyAddresses       []string
	DurationToTableName    map[time.Duration]string
	BlockTableNameFallback string
	PollingInterval        time.Duration
	CleanupInterval        time.Duration
	IdleTimeout            time.Duration
	OutOfOrderTolerance    time.Duration
	LogLevel               string
	MaxTimeSinceLastHit    time.Duration
}

// --- YAML DATA STRUCTURES ---

type StepDefYAML struct {
	Order               int
	FieldMatches        map[string]string `yaml:"field_matches"`
	MaxDelay            string            `yaml:"max_delay"`
	MinDelay            string            `yaml:"min_delay"`
	MinTimeSinceLastHit string            `yaml:"min_time_since_last_hit"` // Renamed from first_hit_since
}

type BehavioralChainYAML struct {
	Name          string        `yaml:"name"`
	Steps         []StepDefYAML `yaml:"steps"`
	Action        string        `yaml:"action"`
	BlockDuration string        `yaml:"block_duration"`
	MatchKey      string        `yaml:"match_key"`
}

type ChainConfig struct {
	Version              string                `yaml:"version"`
	LogLevel             string                `yaml:"log_level"`
	PollingInterval      string                `yaml:"poll_interval"`
	CleanupInterval      string                `yaml:"cleanup_interval"`
	IdleTimeout          string                `yaml:"idle_timeout"`
	OutOfOrderTolerance  string                `yaml:"out_of_order_tolerance"`
	Chains               []BehavioralChainYAML `yaml:"chains"`
	WhitelistCIDRs       []string              `yaml:"whitelist_cidrs"`
	HAProxyAddresses     []string              `yaml:"haproxy_addresses"`
	DurationTables       map[string]string     `yaml:"duration_tables"`
	DefaultBlockDuration string                `yaml:"default_block_duration"`
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
	FieldMatches        map[string]string
	MaxDelayDuration    time.Duration
	MinDelayDuration    time.Duration
	MinTimeSinceLastHit time.Duration             // Renamed from FirstHitSinceDuration
	CompiledRegexes     map[string]*regexp.Regexp // Pre-compiled regexes for performance.
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
