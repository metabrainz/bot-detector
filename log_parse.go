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

	statusCodeIndex := getMatchIndex("StatusCode")
	statusCode, _ := strconv.Atoi(matches[statusCodeIndex])

	timeIndex := getMatchIndex("Timestamp")
	t, err := time.Parse(logTimeFormat, matches[timeIndex])
	if err != nil {
		return nil, fmt.Errorf("failed to parse log time: %w", err)
	}

	ipIndex := getMatchIndex("IP")
	ipAddress := matches[ipIndex]

	ipVersion := GetIPVersion(ipAddress)

	entry := &LogEntry{
		Timestamp:  t,
		IP:         ipAddress,
		Path:       matches[getMatchIndex("Path")],
		Method:     matches[getMatchIndex("Method")],
		Protocol:   matches[getMatchIndex("Protocol")],
		UserAgent:  matches[getMatchIndex("UserAgent")],
		Referrer:   matches[getMatchIndex("Referrer")],
		StatusCode: statusCode,
		IPVersion:  ipVersion,
	}

	return entry, nil
}

func (p *Processor) ProcessLogLine(line string, lineNumber int) {
	entry, err := ParseLogLine(line)

	if err != nil && entry == nil {
		if isTimeParseErr := (err.Error() == "failed to parse log time: time: unknown format"); isTimeParseErr {
			p.LogFunc(LevelWarning, "PARSE_FAIL", "Line: %s - Failed to parse log time: %v. Using current time for entry.", line, err)

			p.LogFunc(LevelError, "PARSE_FAIL", "Failed to parse log line: %s (Error: %v)", line, err)
			return

		} else {
			p.LogFunc(LevelError, "PARSE_FAIL", "Failed to parse log line: %s (Error: %v)", line, err)
			return
		}
	}

	if entry == nil {
		return
	}

	if p.IsWhitelistedFunc(entry.IP) {
		p.LogFunc(LevelDebug, "SKIP", "IP %s: Skipped (Whitelisted).", entry.IP)
		return
	}

	ipOnlyKey := TrackingKey{IP: entry.IP, UA: ""}
	store := p.ActivityStore
	mutex := p.ActivityMutex

	mutex.Lock()
	activity := GetOrCreateActivityUnsafe(store, ipOnlyKey)

	if activity.IsBlocked && time.Now().After(activity.BlockedUntil) {
		p.LogFunc(LevelInfo, "EXPIRE", "In-memory block expired for IP %s.", entry.IP)
		activity.IsBlocked = false
		activity.BlockedUntil = time.Time{}
	}

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
	activity = GetOrCreateActivityUnsafe(store, ipOnlyKey)
	activity.LastRequestTime = entry.Timestamp
	mutex.Unlock()
}

func ProcessLogLine(line string, lineNumber int) {
	// 1. Determine which store/mutex to use based on the global DryRun flag.
	var store map[TrackingKey]*BotActivity
	var mutex *sync.RWMutex // Correct type for assignment

	if DryRun {
		store = DryRunActivityStore
		mutex = &DryRunActivityMutex
	} else {
		store = ActivityStore
		mutex = &ActivityMutex
	}

	// 2. Instantiate the Processor struct, using globals for fields.
	p := &Processor{
		ActivityStore:     store,
		ActivityMutex:     mutex,
		Chains:            Chains,
		ChainMutex:        &ChainMutex,
		DryRun:            DryRun,
		LogFunc:           LogOutput,
		IsWhitelistedFunc: IsIPWhitelisted,
		Blocker:           &GlobalBlocker{},
	}

	// 3. Call the new, testable method.
	p.ProcessLogLine(line, lineNumber)
}
