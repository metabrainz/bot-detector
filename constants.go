package main

import "errors"

// --- CONSTANT FOR CRITICAL LOG LINE BUFFER LIMIT ---
// If a line exceeds this limit (e.g., 16KB), it is skipped entirely.
const MaxLogLineSize = 16 * 1024

var SupportedConfigVersions = []string{"1.0"}

// Custom error type for skipped lines
var ErrLineSkipped = errors.New("line exceeded critical limit and was skipped")
