package main

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Regex for parsing the extended log format (VHost + Combined Log Format).
// Example: musicbrainz.org 10.0.0.1 - - [28/Oct/2025:17:00:00 +0000] "GET /ping HTTP/1.1" 200 100 "-" "Baseline"
// Uses named capture groups for robustness.
var logRegex = regexp.MustCompile(
	`^(?P<VHost>\S+) (?P<IP>\S+) (?P<Identity>\S+) (?P<User>\S+) \[(?P<Timestamp>[^\]]+)\] \"(?P<Method>\S+) (?P<Path>\S+) (?P<Protocol>\S+)\" (?P<StatusCode>\d{3}) (?P<Size>\d+) \"(?P<Referrer>[^\"]*)\" \"(?P<UserAgent>[^\"]*)\"$`,
)

// Time format used in standard logs
const logTimeFormat = "02/Jan/2006:15:04:05 -0700"

// ParseLogLine converts a raw log string into a structured LogEntry.
func ParseLogLine(line string) (*LogEntry, error) {
	// Skip comments and truly empty lines.
	// NOTE: Lines containing only whitespace will proceed to the regex match
	// and fail, correctly triggering a PARSE_FAIL error.
	if len(line) == 0 || line[0] == '#' {
		return nil, nil // Return nil to signify the line was skipped gracefully
	}

	matches := logRegex.FindStringSubmatch(line)
	if matches == nil {
		return nil, fmt.Errorf("line does not match log format regex")
	}

	// Dynamically determine the expected number of match elements.
	// logRegex.NumSubexp() returns the count of capturing groups.
	// The match slice includes the full match at index 0, so length = groups + 1.
	expectedLength := logRegex.NumSubexp() + 1

	// Check the match count dynamically against the compiled regex.
	if len(matches) != expectedLength {
		return nil, fmt.Errorf("malformed essential fields: expected %d groups, got %d", logRegex.NumSubexp(), len(matches)-1)
	}

	// Helper to retrieve the slice index by capture group name
	getMatchIndex := func(name string) int {
		return logRegex.SubexpIndex(name)
	}

	// Parse status code
	statusCodeIndex := getMatchIndex("StatusCode")
	statusCode, _ := strconv.Atoi(matches[statusCodeIndex])
	// Note: We ignore errors here; statusCode defaults to 0 if Atoi fails,
	// which is acceptable as it likely won't match a chain.

	// Parse timestamp
	timeIndex := getMatchIndex("Timestamp")
	t, err := time.Parse(logTimeFormat, matches[timeIndex])
	if err != nil {
		// Log the error but use time.Now() as a fallback for the timestamp
		LogOutput(LevelWarning, "PARSE_FAIL", "Line: %s - Failed to parse log time: %v. Using current time for entry.", line, err)
		t = time.Now().UTC()
	}

	// Create LogEntry using named indices
	entry := &LogEntry{
		Timestamp:  t,
		IP:         matches[getMatchIndex("IP")],
		Path:       matches[getMatchIndex("Path")],
		Method:     matches[getMatchIndex("Method")],
		Protocol:   matches[getMatchIndex("Protocol")],
		UserAgent:  matches[getMatchIndex("UserAgent")],
		Referrer:   matches[getMatchIndex("Referrer")],
		StatusCode: statusCode,
	}

	return entry, nil
}

// ProcessLogLine is responsible for parsing the line and checking all behavioral chains.
func ProcessLogLine(line string, lineNumber int) {
	entry, err := ParseLogLine(line)

	if err != nil {
		// Only log unrecoverable parse errors, not nil errors from skipped lines (comments/empty).
		LogOutput(LevelError, "PARSE_FAIL", "Failed to parse log line: %s (Error: %v)", line, err)
		return
	}

	// If entry is nil, it means ParseLogLine silently skipped it (e.g., comment or empty line)
	if entry == nil {
		return
	}

	// 1. Check if IP is whitelisted
	if IsIPWhitelisted(entry.IP) {
		LogOutput(LevelDebug, "SKIP", "IP %s: Skipped (Whitelisted).", entry.IP)
		return
	}

	// 2. Check if IP is currently blocked (in-memory state)
	// We use the IP-only key for global block/skip checks.
	ipOnlyKey := TrackingKey{IP: entry.IP, UA: ""}

	// Choose appropriate store & mutex based on mode.
	store := ActivityStore
	mutex := &ActivityMutex
	if DryRun {
		store = DryRunActivityStore
		mutex = &DryRunActivityMutex
	}

	mutex.Lock()
	// GetOrCreateActivityUnsafe is used because we hold the lock.
	activity := GetOrCreateActivityUnsafe(store, ipOnlyKey)

	// If the IP is blocked, check if the block has expired
	if activity.IsBlocked && time.Now().After(activity.BlockedUntil) {
		LogOutput(LevelInfo, "EXPIRE", "In-memory block expired for IP %s.", entry.IP)
		activity.IsBlocked = false
		activity.BlockedUntil = time.Time{}
	}

	// If still blocked, skip further chain checks
	if activity.IsBlocked {
		LogOutput(LevelDebug, "SKIP", "IP %s: Skipped (Already blocked in memory).", entry.IP)
		activity.LastRequestTime = entry.Timestamp // Update time even if skipped
		mutex.Unlock()
		return
	}

	// Update LastRequestTime for the IP-only key before unlocking, even if no chains match.
	activity.LastRequestTime = entry.Timestamp
	mutex.Unlock()

	// 3. Process the log line through all behavioral chains
	CheckChains(entry)
}
