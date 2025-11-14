package parser

import (
	"bot-detector/internal/utils"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Provider defines the interface that the parser needs to access configuration.
type Provider interface {
	GetTimestampFormat() string
	GetLogRegex() *regexp.Regexp
}

// LogEntry is a local copy of the LogEntry struct to avoid circular dependencies.
// The main application will convert this to its own LogEntry type.
type LogEntry struct {
	Timestamp  time.Time
	IPInfo     utils.IPInfo
	Method     string
	Path       string
	Protocol   string
	Referrer   string
	StatusCode int
	Size       int
	UserAgent  string
	VHost      string
}

var defaultLogRegex = regexp.MustCompile(
	`^(?P<VHost>\S+) (?P<IP>\S+) (?P<Identity>\S+) (?P<User>\S+) \[(?P<Timestamp>[^\]]+)\] (?:\"(?P<Method>\S+)\s+(?P<Path>.+?)\s+(?P<Protocol>\S+)\"|\"-\"|-) (?P<StatusCode>\d{1,3}) (?P<Size>\S+) \"(?P<Referrer>[^\"]*)\" \"(?P<UserAgent>[^\"]*)\"$`,
)

// getMatch safely retrieves a value from a named capture group.
func getMatch(name string, matches []string, regex *regexp.Regexp) string {
	idx := regex.SubexpIndex(name)
	if idx == -1 || idx >= len(matches) {
		return ""
	}
	return matches[idx]
}

// ParseLogLine parses a single log line string into a structured LogEntry.
func ParseLogLine(p Provider, line string) (*LogEntry, error) {
	if len(line) == 0 || line[0] == '#' {
		return nil, nil
	}

	regexToUse := p.GetLogRegex()
	if regexToUse == nil {
		regexToUse = defaultLogRegex
	}

	matches := regexToUse.FindStringSubmatch(line)
	if matches == nil {
		return nil, fmt.Errorf("line does not match log format regex")
	}

	ipStr := getMatch("IP", matches, regexToUse)
	timestampStr := getMatch("Timestamp", matches, regexToUse)
	timestamp, err := time.Parse(p.GetTimestampFormat(), timestampStr)
	if err != nil {
		return nil, fmt.Errorf("malformed timestamp: %w", err)
	}

	ipInfo := utils.NewIPInfo(ipStr)
	if ipInfo.Version == utils.VersionInvalid {
		return nil, fmt.Errorf("invalid or unrecognized IP address '%s'", ipStr)
	}

	statusCode, _ := strconv.Atoi(getMatch("StatusCode", matches, regexToUse))

	var size int
	sizeStr := getMatch("Size", matches, regexToUse)
	if sizeStr == "-" {
		// A dash for size often indicates a request with no response body (e.g., 204, 304, or a failed request).
		size = -1
	} else {
		size, _ = strconv.Atoi(sizeStr)
	}

	return &LogEntry{
		Timestamp:  timestamp,
		IPInfo:     ipInfo,
		Method:     utils.ForLog(getMatch("Method", matches, regexToUse)),
		Path:       utils.ForLog(getMatch("Path", matches, regexToUse)),
		Protocol:   utils.ForLog(getMatch("Protocol", matches, regexToUse)),
		Referrer:   utils.ForLog(getMatch("Referrer", matches, regexToUse)),
		StatusCode: statusCode,
		UserAgent:  utils.ForLog(getMatch("UserAgent", matches, regexToUse)),
		Size:       size,
		VHost:      utils.ForLog(getMatch("VHost", matches, regexToUse)),
	}, nil
}
