package main

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// defaultLogRegex is the fallback regex for parsing the extended log format (VHost + Combined Log Format).
var defaultLogRegex = regexp.MustCompile(
	`^(?P<VHost>\S+) (?P<IP>\S+) (?P<Identity>\S+) (?P<User>\S+) \[(?P<Timestamp>[^\]]+)\] \"(?P<Method>\S+) (?P<Path>\S+) (?P<Protocol>\S+)\" (?P<StatusCode>\d{1,3}) (?P<Size>\d+) \"(?P<Referrer>[^\"]*)\" \"(?P<UserAgent>[^\"]*)\"$`,
)

// AccessLogTimeFormat defines the timestamp format used in standard access logs.
const AccessLogTimeFormat = "02/Jan/2006:15:04:05 -0700"

// getMatch safely retrieves a value from a named capture group.
// If the group name does not exist in the regex, it returns an empty string.
func getMatch(name string, matches []string, regex *regexp.Regexp) string {
	idx := regex.SubexpIndex(name)
	if idx == -1 || idx >= len(matches) {
		return ""
	}
	return matches[idx]
}

func (p *Processor) ParseLogLine(line string) (*LogEntry, error) {
	if len(line) == 0 || line[0] == '#' {
		return nil, nil
	}

	// Determine which regex to use.
	regexToUse := p.LogRegex
	if regexToUse == nil {
		regexToUse = defaultLogRegex
	}

	matches := regexToUse.FindStringSubmatch(line)
	if matches == nil {
		return nil, fmt.Errorf("line does not match log format regex")
	}

	// These are guaranteed to exist by the config loader validation.
	ipStr := getMatch("IP", matches, regexToUse)
	timestampStr := getMatch("Timestamp", matches, regexToUse)
	timestamp, err := time.Parse(p.Config.TimestampFormat, timestampStr)
	if err != nil {
		return nil, fmt.Errorf("malformed timestamp: %w", err)
	}

	ipInfo := NewIPInfo(ipStr)
	if ipInfo.Version == VersionInvalid {
		return nil, fmt.Errorf("invalid or unrecognized IP address '%s'", ipStr)
	}

	statusCode, _ := strconv.Atoi(getMatch("StatusCode", matches, regexToUse))

	return &LogEntry{
		Timestamp:  timestamp, // Keep timestamp first as it's the primary time axis.
		IPInfo:     ipInfo,
		Method:     getMatch("Method", matches, regexToUse),
		Path:       getMatch("Path", matches, regexToUse),
		Protocol:   getMatch("Protocol", matches, regexToUse),
		Referrer:   getMatch("Referrer", matches, regexToUse),
		StatusCode: statusCode,
		UserAgent:  getMatch("UserAgent", matches, regexToUse),
	}, nil
}

func processLogLineInternal(p *Processor, line string, lineNumber int) {
	// 1. Parse the line
	entry, err := p.ParseLogLine(line)

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
	p.CheckChainsFunc(entry)
}
