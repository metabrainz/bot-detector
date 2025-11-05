package main

import (
	"regexp"
	"sync"
	"time"
)

// --- EXTERNAL INTERFACES ---

// IPVersion is used internally to track whether an IP is v4 or v6.
type IPVersion byte

// Blocker defines the interface for external IP blocking services (e.g., HAProxy).
type Blocker interface {
	Block(ip string, version IPVersion, duration time.Duration) error
}

// --- DEPENDENCY CONTAINER ---

// Processor holds all necessary dependencies and state for log processing,
// making it easy to mock/stub external calls and manage state in tests.
type Processor struct {
	ActivityStore     map[TrackingKey]*BotActivity
	ActivityMutex     *sync.RWMutex
	Chains            []BehavioralChain
	ChainMutex        *sync.RWMutex // Assuming ChainMutex is still needed for Chain loading
	DryRun            bool
	LogFunc           func(level LogLevel, tag string, format string, args ...interface{})
	IsWhitelistedFunc func(ip string) bool
	// The Blocker interface allows injecting the real HAProxy implementation or a mock for tests.
	Blocker Blocker
}

// --- YAML DATA STRUCTURES ---

type StepDefYAML struct {
	Order        int
	FieldMatches map[string]string `yaml:"field_matches"`
	MaxDelay     string            `yaml:"max_delay"`
	MinDelay     string            `yaml:"min_delay"`
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
	Chains               []BehavioralChainYAML `yaml:"chains"`
	WhitelistCIDRs       []string              `yaml:"whitelist_cidrs"`
	HAProxyAddresses     []string              `yaml:"haproxy_addresses"`
	DurationTables       map[string]string     `yaml:"duration_tables"`
	DefaultBlockDuration string                `yaml:"default_block_duration"`
}

// --- RUNTIME DATA STRUCTURES ---

type LogEntry struct {
	Timestamp  time.Time // Actual time of the request (parsed from log, not time.Now()).
	IP         string
	Path       string
	Method     string
	Protocol   string
	UserAgent  string
	Referrer   string
	StatusCode int
	IPVersion  IPVersion
}

type StepDef struct {
	Order            int
	FieldMatches     map[string]string
	MaxDelayDuration time.Duration
	MinDelayDuration time.Duration
	CompiledRegexes  map[string]*regexp.Regexp // Pre-compiled regexes for performance.
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
	IP string
	UA string // UserAgent. Empty string if tracking is IP-only.
}

// BotActivity tracks state for a single IP address (or IP+UA combination) across all chains.
type BotActivity struct {
	LastRequestTime time.Time // Time of the IP's PREVIOUS overall request.
	ChainProgress   map[string]StepState
	IsBlocked       bool      // Flag to skip chain checks if this key is blocked.
	BlockedUntil    time.Time // Time when the block expires.
}
