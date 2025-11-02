package main

import (
	"regexp"
	"time"
)

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
