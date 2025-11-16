package logging

import (
	"log"
	"strings"
	"sync"
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

var (
	currentLogLevel = LevelInfo // Default level set to INFO
	logMutex        sync.RWMutex
)

var logLevelMap = map[string]LogLevel{
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
	logMutex.RLock()
	defer logMutex.RUnlock()
	if level <= currentLogLevel {
		log.Printf("[%s] "+format, append([]interface{}{prefix}, v...)...)
	}
}

// SetLogLevel safely sets the global CurrentLogLevel from a string.
func SetLogLevel(levelStr string) {
	logMutex.Lock()
	defer logMutex.Unlock()
	if level, ok := logLevelMap[strings.ToLower(levelStr)]; ok {
		currentLogLevel = level
	} else {
		// We can't call LogOutput here because it would cause a deadlock.
		// Instead, we'll just log directly.
		log.Printf("[CONFIG] Invalid log_level '%s' in config. Using default 'warning'.", levelStr)
		currentLogLevel = LevelWarning
	}
}

// GetLogLevel is a new exported function to allow other packages to read the current level.
func GetLogLevel() LogLevel {
	logMutex.RLock()
	defer logMutex.RUnlock()
	return currentLogLevel
}
