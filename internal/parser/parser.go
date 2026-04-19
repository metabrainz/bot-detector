package parser

import (
	"bot-detector/internal/types"
	"bot-detector/internal/utils"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// Provider defines the interface that the parser needs to access configuration.
type Provider interface {
	GetTimestampFormat() string
	GetLogRegex() *regexp.Regexp
}

// DefaultLogFormatRegex is the default regex for parsing virtual-host-prefixed combined log format.
var DefaultLogFormatRegex = regexp.MustCompile(
	`^(?P<VHost>\S+) (?P<IP>\S+) (?P<Identity>\S+) (?P<User>\S+) \[(?P<Timestamp>[^\]]+)\] (?:\"(?P<Method>\S+)\s+(?P<Path>.+?)(?:\s+(?P<Protocol>\S+))?\"|\"-\"|\"\"|-) (?P<StatusCode>\d{1,3}) (?P<Size>\S+) \"(?P<Referrer>[^\"]*)\" \"(?P<UserAgent>[^\"]*)\"$`,
)

// subexpIndices caches the named capture group indices for a regex.
type subexpIndices struct {
	vhost, ip, timestamp, method, path, protocol, statusCode, size, referrer, userAgent int
}

// buildSubexpIndices pre-computes named capture group indices for a regex.
func buildSubexpIndices(re *regexp.Regexp) subexpIndices {
	return subexpIndices{
		vhost:      re.SubexpIndex("VHost"),
		ip:         re.SubexpIndex("IP"),
		timestamp:  re.SubexpIndex("Timestamp"),
		method:     re.SubexpIndex("Method"),
		path:       re.SubexpIndex("Path"),
		protocol:   re.SubexpIndex("Protocol"),
		statusCode: re.SubexpIndex("StatusCode"),
		size:       re.SubexpIndex("Size"),
		referrer:   re.SubexpIndex("Referrer"),
		userAgent:  re.SubexpIndex("UserAgent"),
	}
}

var defaultSubexpIndices = buildSubexpIndices(DefaultLogFormatRegex)

// cachedIndices stores pre-computed indices for custom regexes.
var cachedIndicesMu sync.Mutex
var cachedIndicesMap = make(map[*regexp.Regexp]subexpIndices)

func getSubexpIndices(re *regexp.Regexp) subexpIndices {
	if re == DefaultLogFormatRegex {
		return defaultSubexpIndices
	}
	cachedIndicesMu.Lock()
	idx, ok := cachedIndicesMap[re]
	if !ok {
		idx = buildSubexpIndices(re)
		cachedIndicesMap[re] = idx
	}
	cachedIndicesMu.Unlock()
	return idx
}

// extractMatch extracts a substring from line using index-based match results.
func extractMatch(line string, indices []int, subexpIdx int) string {
	if subexpIdx < 0 || 2*subexpIdx+1 >= len(indices) {
		return ""
	}
	start, end := indices[2*subexpIdx], indices[2*subexpIdx+1]
	if start < 0 {
		return ""
	}
	return line[start:end]
}

// ParseLogLine parses a single log line string into a structured LogEntry.
func ParseLogLine(p Provider, line string) (*types.LogEntry, error) {
	if len(line) == 0 || line[0] == '#' {
		return nil, nil
	}

	regexToUse := p.GetLogRegex()
	if regexToUse == nil {
		regexToUse = DefaultLogFormatRegex
	}

	indices := regexToUse.FindStringSubmatchIndex(line)
	if indices == nil {
		return nil, fmt.Errorf("line does not match log format regex")
	}

	idx := getSubexpIndices(regexToUse)

	ipStr := extractMatch(line, indices, idx.ip)
	timestampStr := extractMatch(line, indices, idx.timestamp)
	timestamp, err := time.Parse(p.GetTimestampFormat(), timestampStr)
	if err != nil {
		return nil, fmt.Errorf("malformed timestamp: %w", err)
	}

	ipInfo := utils.NewIPInfo(ipStr)
	if ipInfo.Version == utils.VersionInvalid {
		return nil, fmt.Errorf("invalid or unrecognized IP address '%s'", ipStr)
	}

	statusCode, _ := strconv.Atoi(extractMatch(line, indices, idx.statusCode))

	var size int
	sizeStr := extractMatch(line, indices, idx.size)
	if sizeStr == "-" {
		size = -1
	} else {
		size, _ = strconv.Atoi(sizeStr)
	}

	return &types.LogEntry{
		Timestamp:  timestamp,
		IPInfo:     ipInfo,
		Method:     utils.ForLog(extractMatch(line, indices, idx.method)),
		Path:       utils.ForLog(extractMatch(line, indices, idx.path)),
		Protocol:   utils.ForLog(extractMatch(line, indices, idx.protocol)),
		Referrer:   utils.ForLog(extractMatch(line, indices, idx.referrer)),
		StatusCode: statusCode,
		UserAgent:  utils.ForLog(extractMatch(line, indices, idx.userAgent)),
		Size:       size,
		VHost:      utils.ForLog(extractMatch(line, indices, idx.vhost)),
	}, nil
}
