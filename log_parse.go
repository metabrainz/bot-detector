package main

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseLogLine processes a raw log line into a LogEntry.
func ParseLogLine(line string) (*LogEntry, error) {
	parts := strings.Split(line, "\"")
	if len(parts) < 6 {
		return nil, fmt.Errorf("malformed log line: expected at least 6 quoted sections (got %d)", len(parts))
	}

	ipPart := strings.Fields(parts[0])
	requestPart := strings.Fields(parts[1])
	statusSizePart := strings.Fields(parts[2])

	// We expect at least 5 fields in ipPart (e.g., 127.0.0.1 - - [time tz]),
	// at least 3 fields in requestPart (e.g., GET /path HTTP/1.1)
	if len(ipPart) < 5 || len(requestPart) < 3 || len(statusSizePart) < 1 {
		return nil, fmt.Errorf("malformed essential fields (missing Protocol, Request, Status, or incomplete Host/Time fields)")
	}

	var ip string
	var timeIndexStart, timeIndexEnd int

	// Determine structure based on field count in the first part:
	if len(ipPart) >= 6 {
		// Format: Hostname IP - - [Time TZ] (e.g., musicbrainz.org 197.3.177.209 ...)
		ip = ipPart[1]
		timeIndexStart = 4
		timeIndexEnd = 5
	} else {
		// Format: IP - - [Time TZ] (e.g., 127.0.0.1 - - ...)
		ip = ipPart[0]
		timeIndexStart = 3
		timeIndexEnd = 4
	}

	method := requestPart[0]
	path := requestPart[1]
	protocol := requestPart[2] // EXTRACT PROTOCOL VERSION
	referrer := parts[3]
	userAgent := parts[5]

	statusCode, err := strconv.Atoi(statusSizePart[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse status code: %w", err)
	}

	timeStrWithBrackets := ipPart[timeIndexStart] + " " + ipPart[timeIndexEnd]
	timeStr := strings.Trim(timeStrWithBrackets, "[]")

	t, parseErr := time.Parse("02/Jan/2006:15:04:05 -0700", timeStr)
	if parseErr != nil {
		LogOutput(LevelWarning, "WARN", "Failed to parse log time '%s'. Using current time: %v", timeStr, parseErr)
		t = time.Now()
	}

	return &LogEntry{
		Timestamp:  t,
		IP:         ip,
		Path:       path,
		Method:     method,
		Protocol:   protocol,
		StatusCode: statusCode,
		Referrer:   referrer,
		UserAgent:  userAgent,
	}, nil
}

// ProcessLogLine processes a single raw log line, handling skipping of empty/comment lines,
// parsing, chain checking, and updating the activity store.
func ProcessLogLine(line string, lineNumber int) {
	// Skip truly empty lines and comments.
	if line == "" || line == "\n" || line == "\r\n" || strings.HasPrefix(line, "#") {
		return
	}

	entry, parseErr := ParseLogLine(line)
	if parseErr != nil {
		logLevel := LevelDebug
		prefix := "PARSE_FAIL"
		if DryRun {
			prefix = "DRYRUN_WARN"
			logLevel = LevelWarning
		}

		lineStart := line
		if len(line) > 60 {
			lineStart = line[:60] + "..."
		}
		LogOutput(logLevel, prefix, "[Line %d] Failed to parse log line: %v (Line start: %s)", lineNumber, parseErr, lineStart)
		return
	}

	// Define the IP-Only key for block status check and last request time update.
	ipOnlyKey := TrackingKey{IP: entry.IP, UA: ""}

	store := ActivityStore
	mutex := &ActivityMutex
	if DryRun {
		store = DryRunActivityStore
		mutex = &DryRunActivityMutex
	}

	// Lock the mutex for atomic access to the store for status check and time updates.
	mutex.Lock()

	// 1. Get/Update IP-only key (for block status check and min_delay Step 1 time).
	ipActivity := GetOrCreateActivityUnsafe(store, ipOnlyKey)
	ipActivity.LastRequestTime = entry.Timestamp

	isBlocked := ipActivity.IsBlocked
	blockedUntil := ipActivity.BlockedUntil

	// Check if block has expired
	if isBlocked && entry.Timestamp.After(blockedUntil) {
		ipActivity.IsBlocked = false
		ipActivity.BlockedUntil = time.Time{}
		isBlocked = false
		LogOutput(LevelDebug, "UNBLOCK_EXPIRY", "IP %s block has expired at %s. Unmarked as blocked.", entry.IP, blockedUntil.Format("2006-01-02 15:04:05 -0700"))
	}

	// Unlock before calling CheckChains, which also locks.
	mutex.Unlock()

	if isBlocked {
		// Log the skip and return immediately. This is the IP-only optimization.
		LogOutput(LevelDebug, "SKIP", "IP %s is currently marked as blocked until %s. Skipping chain checks.", entry.IP, blockedUntil.Format("2006-01-02 15:04:05 -0700"))
		return
	}

	// Only proceed to check chains if the IP is not blocked.
	CheckChains(entry)
}

// ReadLineWithLimit reads a line from the bufio.Reader until a newline is found or the limit is hit.
// It uses r.Read(b) for robust final EOF detection, preventing the hang when reading a static file.
func ReadLineWithLimit(r *bufio.Reader, maxBytes int) (string, error) {
	var lineBuilder strings.Builder
	bytesRead := 0

	// Use a 1-byte buffer to read instead of ReadByte().
	b := make([]byte, 1)

	for {
		n, err := r.Read(b)

		if err != nil {
			if n > 0 {
				lineBuilder.Write(b[:n])
			}
			return lineBuilder.String(), err
		}

		// n will always be 1 here for a successful read
		char := b[0]

		if char == '\n' {
			return lineBuilder.String(), nil // Full line found
		}

		lineBuilder.WriteByte(char)
		bytesRead++

		if bytesRead > maxBytes {
			// CRITICAL LIMIT HIT. Drain the rest of the line until the next \n.
			for {
				b_drain, err := r.ReadByte()
				if err != nil {
					// We hit EOF or an I/O error while draining.
					return "", fmt.Errorf("%w (draining failed with error: %v)", ErrLineSkipped, err)
				}
				if b_drain == '\n' {
					break // Successfully drained the rest of the line.
				}
			}
			// Return the skip error. The reader is now correctly positioned at the start of the next line.
			return "", ErrLineSkipped
		}
	}
}
