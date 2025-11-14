package main

import (
	"errors"
	"time"
)

// AppVersion is the application's version string.
const AppVersion = "0.1"

// --- CONSTANT FOR CRITICAL LOG LINE BUFFER LIMIT ---
const MaxLogLineSize = 16 * 1024

var SupportedConfigVersions = []string{"1.0"}

// Custom error type for skipped lines
var ErrLineSkipped = errors.New("line exceeded critical limit and was skipped")

// Delays
const (
	FileOpenRetryDelay = 100 * time.Millisecond // For quick polling when the file is missing (e.g., just after rotation)
	EOFPollingDelay    = 200 * time.Millisecond // For regular polling when hitting EOF on an open file
	ErrorRetryDelay    = 1 * time.Second        // For persistent errors (read failures, stat failures)
)

// AppLogTimestampFormat defines the standard timestamp format for this application's own log output.
const AppLogTimestampFormat = time.RFC3339 // e.g., "2006-01-02T15:04:05Z07:00"

// Default configuration values used if not specified in the YAML file.
const (
	DefaultLogLevel            = "warning"
	DefaultPollingInterval     = "5s"
	DefaultCleanupInterval     = "1m"
	DefaultEOFPollingDelay     = "200ms"
	DefaultIdleTimeout         = "30m"
	DefaultLineEnding          = "lf"
	DefaultOutOfOrderTolerance = "5s"
	DefaultMinPollingInterval  = 1 * time.Second // Minimum safe polling interval to prevent tight loops.

	// Default Blocker client settings
	DefaultBlockerMaxRetries        = 3
	DefaultBlockerRetryDelay        = 200 * time.Millisecond
	DefaultBlockerDialTimeout       = 5 * time.Second
	DefaultBlockerCommandQueueSize  = 1000 // Default queue size
	DefaultBlockerCommandsPerSecond = 10   // Default rate limit
	DefaultUnblockCooldown          = "5m"
)

// Define constants for Top N table formatting
const (
	TopNHeaderFormat = "  %6s %6s %6s %6s %s"
	TopNRowFormat    = "  %6d %6d %6d %6s %s"
)
