package logparser

import (
	"bot-detector/internal/app"
	"bot-detector/internal/logging"
	"bot-detector/internal/parser"
	"bot-detector/internal/utils"
	"sync/atomic"
)

// AccessLogTimeFormat defines the timestamp format used in standard access logs.
const AccessLogTimeFormat = "02/Jan/2006:15:04:05 -0700"

// ProcessLogLineInternal processes a single line of log input.
// Exported for use in tests.
func ProcessLogLineInternal(p *app.Processor, line string) {
	ProcessLogLineWithWebsite(p, line, "")
}

// ProcessLogLineWithWebsite processes a single line with an explicit website name.
// This is used in multi-website mode to avoid race conditions.
func ProcessLogLineWithWebsite(p *app.Processor, line string, website string) {
	// 1. Parse the line
	parsedEntry, err := parser.ParseLogLine(p, line)

	if err != nil {
		// Downgrade parse failures to debug during testing, as they are expected in some tests.
		logLevel := logging.LevelError
		if utils.IsTesting() {
			logLevel = logging.LevelDebug
		}
		if website != "" {
			p.LogFunc(logLevel, "PARSE_FAIL", "Parsing failed for line \"%s\" on website '%s': %v", line, website, err)
		} else {
			p.LogFunc(logLevel, "PARSE_FAIL", "Parsing failed for line \"%s\": %v", line, err)
		}
		p.Metrics.ParseErrors.Add(1)
		return
	}

	// Skip comments and empty lines. ParseLogLine returns (nil, nil) for these.
	if parsedEntry == nil {
		return
	}

	// Convert from parser.LogEntry to types.LogEntry
	entry := &app.LogEntry{
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

	// Set website from parameter (multi-website mode) or from CurrentWebsite (legacy)
	if website != "" {
		entry.Website = website
	} else if len(p.Websites) > 0 {
		p.LogPathMutex.Lock()
		entry.Website = p.CurrentWebsite
		p.LogPathMutex.Unlock()
	}

	// Track per-website line parsing
	if entry.Website != "" {
		p.Metrics.WebsiteLinesParsed.LoadOrStore(entry.Website, new(atomic.Int64))
		val, _ := p.Metrics.WebsiteLinesParsed.Load(entry.Website)
		val.(*atomic.Int64).Add(1)
	}

	// 3. If not blocked, process the log line through all behavioral chains
	p.CheckChainsFunc(entry)
}
