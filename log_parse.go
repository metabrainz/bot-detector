package main

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Regex for parsing the Combined Log Format (or similar)
// Example: 1.2.3.4 - - [30/Oct/2025:10:00:00 +0000] "GET /path HTTP/1.1" 200 1234 "referrer" "user agent"
var logRegex = regexp.MustCompile(
	`^(\S+) (\S+) (\S+) \[([^\]]+)\] "(\S+) (\S+) (\S+)" (\d{3}) (\d+) "([^"]*)" "([^"]*)"`,
)

// Time format used in standard logs
const logTimeFormat = "02/Jan/2006:15:04:05 -0700"

// ParseLogLine converts a raw log string into a structured LogEntry.
func ParseLogLine(line string) (*LogEntry, error) {
	matches := logRegex.FindStringSubmatch(line)

	if len(matches) < 12 {
		return nil, fmt.Errorf("log line does not match expected format")
	}

	// Parse status code
	statusCode, _ := strconv.Atoi(matches[8])
	// Note: We ignore errors here; statusCode defaults to 0 if Atoi fails,
	// which is acceptable as it likely won't match a chain.

	// Parse timestamp
	t, err := time.Parse(logTimeFormat, matches[4])
	if err != nil {
		// Log the error but use current time as a fallback
		LogOutput(LevelWarning, "PARSE_FAIL", "Failed to parse log time '%s'. Using current time: %v", matches[4], err)
		t = time.Now()
	}

	entry := &LogEntry{
		IP:         matches[1],
		Timestamp:  t,
		Method:     matches[5],
		Path:       matches[6],
		Protocol:   matches[7],
		StatusCode: statusCode,
		Referrer:   matches[10],
		UserAgent:  matches[11],
	}

	return entry, nil
}

// ProcessLogLine is the main entry point for handling a single log line.
// It parses the line, checks whitelists/blocklists, and triggers chain checks.
// The '_ int' argument is added to satisfy the caller's signature (too many arguments).
func ProcessLogLine(line string, _ int) {
	entry, err := ParseLogLine(line)
	if err != nil {
		LogOutput(LevelWarning, "PARSE_FAIL", "Failed to parse log line: %s (Error: %v)", line, err)
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

	// Update LastRequestTime *before* checking chains.
	// This ensures the MinDelay check in CheckChains uses the correct time.
	activity.LastRequestTime = entry.Timestamp
	mutex.Unlock() // Unlock before running chain checks (which lock internally)

	// 3. Check chains
	CheckChains(entry)
}
