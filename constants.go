package main

import (
	"errors"
	"time"
)

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
