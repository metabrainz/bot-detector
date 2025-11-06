package main

import (
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// Regex for parsing the extended log format (VHost + Combined Log Format).
var logRegex = regexp.MustCompile(
	`^(?P<VHost>\S+) (?P<IP>\S+) (?P<Identity>\S+) (?P<User>\S+) \[(?P<Timestamp>[^\]]+)\] \"(?P<Method>\S+) (?P<Path>\S+) (?P<Protocol>\S+)\" (?P<StatusCode>\d{3}) (?P<Size>\d+) \"(?P<Referrer>[^\"]*)\" \"(?P<UserAgent>[^\"]*)\"$`,
)

// Time format used in standard logs
const logTimeFormat = "02/Jan/2006:15:04:05 -0700"

func ParseLogLine(line string) (*LogEntry, error) {
	if len(line) == 0 || line[0] == '#' {
		return nil, nil
	}

	matches := logRegex.FindStringSubmatch(line)
	if matches == nil {
		return nil, fmt.Errorf("line does not match log format regex")
	}

	expectedLength := logRegex.NumSubexp() + 1
	if len(matches) != expectedLength {
		return nil, fmt.Errorf("malformed essential fields: expected %d groups, got %d", logRegex.NumSubexp(), len(matches)-1)
	}

	getMatchIndex := func(name string) int {
		return logRegex.SubexpIndex(name)
	}

	statusCode, err := strconv.Atoi(matches[getMatchIndex("StatusCode")])
	if err != nil {
		return nil, fmt.Errorf("malformed status code: %w", err)
	}

	timestamp, err := time.Parse(logTimeFormat, matches[getMatchIndex("Timestamp")])
	if err != nil {
		return nil, fmt.Errorf("malformed timestamp: %w", err)
	}

	ipStr := matches[getMatchIndex("IP")]
	// FIX: Check if the IP is valid immediately.
	ipVersion := GetIPVersion(ipStr)

	if ipVersion == VersionInvalid {
		return nil, fmt.Errorf("invalid or unrecognized IP address '%s'", ipStr)
	}
	// END FIX

	return &LogEntry{
		Timestamp:  timestamp,
		IP:         ipStr,
		Path:       matches[getMatchIndex("Path")],
		Method:     matches[getMatchIndex("Method")],
		Protocol:   matches[getMatchIndex("Protocol")],
		UserAgent:  matches[getMatchIndex("UserAgent")],
		Referrer:   matches[getMatchIndex("Referrer")],
		StatusCode: statusCode,
		IPVersion:  ipVersion, // Use the checked version
	}, nil
}

// ProcessLogLine is the main entry point for processing a single log line.
func (p *Processor) ProcessLogLine(line string, lineNumber int) {
	// 1. Parse the line
	entry, err := ParseLogLine(line)

	if err != nil {
		p.LogFunc(LevelError, "PARSE_FAIL", "Line %d: Parsing failed: %v", lineNumber, err)
		return
	}
	// Skip comments and empty lines
	if entry == nil {
		p.LogFunc(LevelDebug, "SKIP", "Line %d: Skipped (Comment/Empty).", lineNumber)
		return
	}

	// Basic checks and skips
	// Note: entry.IPVersion is checked inside ParseLogLine now, so this check should theoretically only
	// catch cases where ParseLogLine was modified to allow invalid versions, or if the calling context
	// doesn't trust the ParseLogLine check. Keeping it here as a safeguard for the processor logic.
	if entry.IPVersion == VersionInvalid {
		p.LogFunc(LevelDebug, "SKIP", "IP %s: Skipped (Invalid IP version).", entry.IP)
		return
	}
	if p.IsWhitelistedFunc(entry.IP) {
		p.LogFunc(LevelDebug, "SKIP", "IP %s: Skipped (Whitelisted).", entry.IP)
		return
	}

	// 2. Check for in-memory block state based on IP-only key
	ipOnlyKey := TrackingKey{IP: entry.IP, UA: ""}

	// Choose appropriate store & mutex based on the Processor's DryRun state.
	var store map[TrackingKey]*BotActivity
	var mutex *sync.RWMutex

	if p.DryRun { // *** FIX: Check DryRun here ***
		store = DryRunActivityStore
		mutex = &DryRunActivityMutex
	} else {
		store = p.ActivityStore
		mutex = p.ActivityMutex
	}

	mutex.Lock()
	// GetOrCreateActivityUnsafe is used because we hold the lock.
	activity := GetOrCreateActivityUnsafe(store, ipOnlyKey)

	// If the IP is blocked, check if the block has expired
	if activity.IsBlocked && time.Now().After(activity.BlockedUntil) {
		p.LogFunc(LevelInfo, "EXPIRE", "In-memory block expired for IP %s.", entry.IP)
		activity.IsBlocked = false
		activity.BlockedUntil = time.Time{}
	}

	// If still blocked, skip further chain checks
	if activity.IsBlocked {
		p.LogFunc(LevelDebug, "SKIP", "IP %s: Skipped (Already blocked in memory).", entry.IP)
		activity.LastRequestTime = entry.Timestamp
		mutex.Unlock()
		return
	}
	mutex.Unlock()

	// 3. Process the log line through all behavioral chains
	p.CheckChains(entry)

	// 4. After chains have run, lock again to update the LastRequestTime
	// Note: CheckChains will use the correct store/mutex based on p.DryRun, so this
	// second lock/unlock must use the correct store/mutex as well.

	// Recalculate store/mutex for the LastRequestTime update
	if p.DryRun {
		mutex = &DryRunActivityMutex
		store = DryRunActivityStore
	} else {
		mutex = p.ActivityMutex
		store = p.ActivityStore
	}

	mutex.Lock()
	// Update LastRequestTime for the IP-only key
	activity = GetOrCreateActivityUnsafe(store, ipOnlyKey) // Re-fetch, just in case a dry-run chain created it.
	activity.LastRequestTime = entry.Timestamp
	mutex.Unlock()
}
