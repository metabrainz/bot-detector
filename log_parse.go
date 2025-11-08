package main

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Regex for parsing the extended log format (VHost + Combined Log Format).
var logRegex = regexp.MustCompile(
	`^(?P<VHost>\S+) (?P<IP>\S+) (?P<Identity>\S+) (?P<User>\S+) \[(?P<Timestamp>[^\]]+)\] \"(?P<Method>\S+) (?P<Path>\S+) (?P<Protocol>\S+)\" (?P<StatusCode>\d{1,3}) (?P<Size>\d+) \"(?P<Referrer>[^\"]*)\" \"(?P<UserAgent>[^\"]*)\"$`,
)

// AccessLogTimeFormat defines the timestamp format used in standard access logs.
const AccessLogTimeFormat = "02/Jan/2006:15:04:05 -0700"

func ParseLogLine(line string) (*LogEntry, error) {
	if len(line) == 0 || line[0] == '#' {
		return nil, nil
	}

	matches := logRegex.FindStringSubmatch(line)
	if matches == nil {
		return nil, fmt.Errorf("line does not match log format regex")
	}

	getMatchIndex := func(name string) int {
		return logRegex.SubexpIndex(name)
	}

	statusCode, _ := strconv.Atoi(matches[getMatchIndex("StatusCode")])
	timestamp, err := time.Parse(AccessLogTimeFormat, matches[getMatchIndex("Timestamp")])
	if err != nil {
		return nil, fmt.Errorf("malformed timestamp: %w", err)
	}

	ipStr := matches[getMatchIndex("IP")]
	ipInfo := NewIPInfo(ipStr)
	if ipInfo.Version == VersionInvalid {
		return nil, fmt.Errorf("invalid or unrecognized IP address '%s'", ipStr)
	}

	return &LogEntry{
		Timestamp:  timestamp, // Keep timestamp first as it's the primary time axis.
		IPInfo:     ipInfo,
		Method:     matches[getMatchIndex("Method")],
		Path:       matches[getMatchIndex("Path")],
		Protocol:   matches[getMatchIndex("Protocol")],
		Referrer:   matches[getMatchIndex("Referrer")],
		StatusCode: statusCode,
		UserAgent:  matches[getMatchIndex("UserAgent")],
	}, nil
}

func processLogLineInternal(p *Processor, line string, lineNumber int) {
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

	// Skip comments and empty lines. ParseLogLine returns (nil, nil) for these.
	if entry == nil {
		p.LogFunc(LevelDebug, "SKIP", "Line %d: Skipped (Comment/Empty).", lineNumber)
		return
	}

	// 3. If not blocked, process the log line through all behavioral chains
	p.CheckChains(entry)
}
