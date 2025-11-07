package main

import (
	"fmt"
	"regexp"
	"strconv"
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
	ipInfo := NewIPInfo(ipStr)
	if ipInfo.Version == VersionInvalid {
		return nil, fmt.Errorf("invalid or unrecognized IP address '%s'", ipStr)
	}

	return &LogEntry{
		Timestamp:  timestamp,
		IPInfo:     ipInfo,
		Path:       matches[getMatchIndex("Path")],
		Method:     matches[getMatchIndex("Method")],
		Protocol:   matches[getMatchIndex("Protocol")],
		UserAgent:  matches[getMatchIndex("UserAgent")],
		Referrer:   matches[getMatchIndex("Referrer")],
		StatusCode: statusCode,
	}, nil
}

// ProcessLogLine is the main entry point for processing a single log line.
func (p *Processor) ProcessLogLine(line string, lineNumber int) {
	// 1. Parse the line
	entry, err := ParseLogLine(line)

	if err != nil {
		// Downgrade parse failures to debug during testing, as they are expected in some tests.
		logLevel := LevelError
		if isTesting() {
			logLevel = LevelDebug
		}
		p.LogFunc(logLevel, "PARSE_FAIL", "Line %d: Parsing failed: %v", lineNumber, err)
		return
	}
	// Skip comments and empty lines
	if entry == nil {
		p.LogFunc(LevelDebug, "SKIP", "Line %d: Skipped (Comment/Empty).", lineNumber)
		return
	}

	// 2. Check for in-memory block state (Optimization)
	ipOnlyKey := TrackingKey{IPInfo: entry.IPInfo, UA: ""}

	p.ActivityMutex.Lock()
	activity := GetOrCreateActivityUnsafe(p.ActivityStore, ipOnlyKey)

	// If the IP is blocked, check if the block has expired
	if activity.IsBlocked && time.Now().After(activity.BlockedUntil) {
		p.LogFunc(LevelInfo, "EXPIRE", "In-memory block expired for IP %s.", entry.IPInfo.Address)
		activity.IsBlocked = false
		activity.BlockedUntil = time.Time{}
	}

	// If still blocked, skip further chain checks
	if activity.IsBlocked {
		p.LogFunc(LevelDebug, "SKIP", "IP %s: Skipped (Already blocked in memory).", entry.IPInfo.Address)
		activity.LastRequestTime = entry.Timestamp // Update timestamp to prevent premature cleanup
		p.ActivityMutex.Unlock()
		return
	}
	p.ActivityMutex.Unlock()

	// 3. If not blocked, process the log line through all behavioral chains
	p.CheckChains(entry)
}
