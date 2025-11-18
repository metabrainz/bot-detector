package logparser

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/parser"
	"bot-detector/internal/utils"
)

// AccessLogTimeFormat defines the timestamp format used in standard access logs.
const AccessLogTimeFormat = "02/Jan/2006:15:04:05 -0700"

func processLogLineInternal(p *Processor, line string) {
	// 1. Parse the line
	parsedEntry, err := parser.ParseLogLine(p, line)

	if err != nil {
		// Downgrade parse failures to debug during testing, as they are expected in some tests.
		logLevel := logging.LevelError
		if IsTesting() {
			logLevel = logging.LevelDebug
		}
		p.LogFunc(logLevel, "PARSE_FAIL", "Parsing failed for line \"%s\": %v", line, err)
		p.Metrics.ParseErrors.Add(1)
		return
	}

	// Skip comments and empty lines. ParseLogLine returns (nil, nil) for these.
	if parsedEntry == nil {
		return
	}

	// Convert from parser.LogEntry to main.LogEntry
	entry := &LogEntry{
		Timestamp:  parsedEntry.Timestamp,
		IPInfo:     utils.NewIPInfo(parsedEntry.IPInfo.Address),
		Method:     parsedEntry.Method,
		Path:       parsedEntry.Path,
		Protocol:   parsedEntry.Protocol,
		Referrer:   parsedEntry.Referrer,
		StatusCode: parsedEntry.StatusCode,
		Size:       parsedEntry.Size,
		UserAgent:  parsedEntry.UserAgent,
		VHost:      parsedEntry.VHost,
	}

	// 3. If not blocked, process the log line through all behavioral chains
	p.CheckChainsFunc(entry)
}
