package main

import (
	"log"
	"strings"
)

// --- LOGGING STRUCTURE ---
type LogLevel int

const (
	LevelCritical LogLevel = iota // 0: Highest priority: Blocks, Fatal errors
	LevelError                    // 1: Critical failure, but program continues
	LevelWarning                  // 2: Non-critical issues, time parse warnings (Default Level)
	LevelInfo                     // 3: Default mode: Startup, shutdown, significant operational events (e.g., config reload)
	LevelDebug                    // 4: Verbose: All high-volume messages (skip, match, reset, cleanup, watch polling)
)

var CurrentLogLevel = LevelWarning // Default level set to WARNING
var LogLevelMap = map[string]LogLevel{
	"critical": LevelCritical,
	"error":    LevelError,
	"warning":  LevelWarning,
	"info":     LevelInfo,
	"debug":    LevelDebug,
}

// String returns the string representation of a LogLevel.
func (l LogLevel) String() string {
	switch l {
	case LevelCritical:
		return "critical"
	case LevelError:
		return "error"
	case LevelWarning:
		return "warning"
	case LevelInfo:
		return "info"
	case LevelDebug:
		return "debug"
	default:
		return "unknown"
	}
}

// LogOutput is a variable holding the current logging function.
// This allows it to be replaced with a mock during testing.
var LogOutput func(level LogLevel, prefix string, format string, v ...interface{})

func init() {
	LogOutput = logOutputInternal // Assign the real implementation at startup.
}

func logOutputInternal(level LogLevel, prefix string, format string, v ...interface{}) {
	if level <= CurrentLogLevel {
		log.Printf("[%s] "+format, append([]interface{}{prefix}, v...)...)
	}
}

// SetLogLevel safely sets the global CurrentLogLevel from a string.
func SetLogLevel(levelStr string) {
	if level, ok := LogLevelMap[strings.ToLower(levelStr)]; ok {
		CurrentLogLevel = level
	} else {
		LogOutput(LevelWarning, "CONFIG", "Invalid log_level '%s' in config. Using default 'warning'.", levelStr)
		CurrentLogLevel = LevelWarning
	}
}
