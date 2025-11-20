package types

import (
	"bot-detector/internal/utils"
	"fmt"
	"strconv"
	"time"
)

// FieldType indicates the native type of a field from a LogEntry.
type FieldType int

const (
	StringField FieldType = iota
	IntField
	UnsupportedField
)

// FieldNameCanonicalMap maps lowercase YAML field names to their canonical PascalCase
// equivalents in the LogEntry struct. This allows for case-insensitive configuration.
var FieldNameCanonicalMap = map[string]string{
	"ip":         "IP",
	"path":       "Path",
	"method":     "Method",
	"protocol":   "Protocol",
	"useragent":  "UserAgent",
	"user_agent": "UserAgent",
	"referrer":   "Referrer",
	"statuscode": "StatusCode",
	"size":       "Size",
	"vhost":      "VHost",
}

// LogEntry represents a parsed log entry with all its fields.
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

	// fieldCache stores extracted field values to avoid redundant GetMatchValue calls.
	// Key: canonical field name (e.g., "Path", "UserAgent")
	// Value: the extracted field value (string or int)
	// This cache is populated lazily during matcher execution.
	fieldCache map[string]interface{}

	// matcherCache stores matcher evaluation results to avoid redundant matcher executions.
	// Key: matcher cache key (e.g., "path:regex:^/login")
	// Value: the boolean result of the matcher evaluation
	// This cache is populated lazily during matcher execution.
	matcherCache map[string]bool
}

// GetMatchValue returns the value and type of a field from a LogEntry.
func GetMatchValue(fieldName string, entry *LogEntry) (interface{}, FieldType, error) {
	// If entry is nil, this is a compile-time check for the field's type.
	if entry == nil {
		entry = &LogEntry{} // Use a zero-value entry to get the type.
	}

	switch fieldName {
	case "IP":
		return entry.IPInfo.Address, StringField, nil
	case "Path":
		return entry.Path, StringField, nil
	case "Method":
		return entry.Method, StringField, nil
	case "Protocol":
		return entry.Protocol, StringField, nil
	case "UserAgent":
		return entry.UserAgent, StringField, nil
	case "Referrer":
		return entry.Referrer, StringField, nil
	case "StatusCode":
		return entry.StatusCode, IntField, nil
	case "Size":
		return entry.Size, IntField, nil
	case "VHost":
		return entry.VHost, StringField, nil
	default:
		return nil, UnsupportedField, fmt.Errorf("unknown field: '%s'", fieldName)
	}
}

// GetMatchValueIfType retrieves a field's value only if it matches the expected type.
// It uses a per-entry cache to avoid redundant field extractions.
func GetMatchValueIfType(fieldName string, entry *LogEntry, expectedType FieldType) interface{} {
	// Early return for nil entry - avoids unnecessary processing
	if entry == nil {
		return nil
	}

	// Build cache key once (includes both field name and expected type)
	cacheKey := fieldName + ":" + strconv.Itoa(int(expectedType))

	// Check cache first
	if entry.fieldCache != nil {
		if cachedVal, ok := entry.fieldCache[cacheKey]; ok {
			return cachedVal // Cache hit - return immediately
		}
	}

	// Cache miss - extract value normally
	value, actualType, err := GetMatchValue(fieldName, entry) //nolint:errcheck
	if err != nil || actualType != expectedType {
		return nil
	}

	// Populate cache for future lookups
	if entry.fieldCache == nil {
		entry.fieldCache = make(map[string]interface{})
	}
	entry.fieldCache[cacheKey] = value

	return value
}

// CheckMatcherCache looks up a cached matcher result.
// Returns (result, found) where found indicates if the key exists in cache.
func (e *LogEntry) CheckMatcherCache(cacheKey string) (bool, bool) {
	if e == nil || e.matcherCache == nil {
		return false, false
	}
	result, found := e.matcherCache[cacheKey]
	return result, found
}

// StoreMatcherResult caches a matcher evaluation result.
func (e *LogEntry) StoreMatcherResult(cacheKey string, result bool) {
	if e == nil {
		return
	}
	if e.matcherCache == nil {
		e.matcherCache = make(map[string]bool)
	}
	e.matcherCache[cacheKey] = result
}
