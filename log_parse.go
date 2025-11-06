package main

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Regex for parsing the extended log format (VHost + Combined Log Format).
var logRegex = regexp.MustCompile(
	`^(?P<VHost>\S+) (?P<IP>\S+) (?P<Identity>\S+) (?P<User>\S+) \[(?P<Timestamp>[^\]]+)\] \"(?P<Method>\S+) (?P<Path>\S+) (?P<Protocol>\S+)\" (?P<StatusCode>\d{3}) (?P<Size>\d+) \"(?P<Referrer>[^"]*)\" \"(?P<UserAgent>[^"]*)\"$`,
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

	timestamp, err := time.Parse(logTimeFormat, matches[getMatchIndex("Timestamp")])
	if err != nil {
		return nil, fmt.Errorf("failed to parse timestamp: %w", err)
	}

	ipStr := matches[getMatchIndex("IP")]
	statusCode, err := strconv.Atoi(matches[getMatchIndex("StatusCode")])
	if err != nil {
		return nil, fmt.Errorf("failed to parse StatusCode: %w", err)
	}

	entry := &LogEntry{
		Timestamp:  timestamp,
		IP:         ipStr,
		Path:       matches[getMatchIndex("Path")],
		Method:     matches[getMatchIndex("Method")],
		Protocol:   matches[getMatchIndex("Protocol")],
		UserAgent:  matches[getMatchIndex("UserAgent")],
		Referrer:   matches[getMatchIndex("Referrer")],
		StatusCode: statusCode,
		IPVersion:  GetIPVersion(ipStr), // Set IPVersion immediately upon parsing
	}

	return entry, nil
}

// ProcessLogLine is refactored as a method on Processor.
func (p *Processor) ProcessLogLine(line string, lineNumber int) {
	// 1. Parse the log line
	entry, err := ParseLogLine(line)
	if err != nil {
		p.LogFunc(LevelError, "PARSE_FAIL", "Line %d: Failed to parse log line: %v -> Raw line: %s", lineNumber, err, line)
		return
	}
	if entry == nil {
		// Line was a comment or truly empty. Skip.
		return
	}
	if entry.IPVersion == VersionInvalid {
		p.LogFunc(LevelDebug, "SKIP", "Line %d: Skipped (IP address is invalid)", lineNumber)
		return
	}
	if p.IsWhitelistedFunc(entry.IP) {
		p.LogFunc(LevelDebug, "SKIP", "IP %s: Skipped (Whitelisted).", entry.IP)
		return
	}

	// 2. Check for in-memory block state based on IP-only key
	ipOnlyKey := TrackingKey{IP: entry.IP, UA: ""}

	// Choose appropriate store & mutex based on the Processor's DryRun state.
	store := p.ActivityStore
	mutex := p.ActivityMutex

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
	mutex.Lock()
	// NOTE: It is possible activity was deleted by CleanUpIdleActivity in the brief period
	// between the first unlock and this lock. We must re-check/re-create.
	activity = GetOrCreateActivityUnsafe(store, ipOnlyKey)
	activity.LastRequestTime = entry.Timestamp
	mutex.Unlock()
}
