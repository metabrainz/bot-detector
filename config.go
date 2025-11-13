package main

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/utils"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// yamlKeyAliases maps various key formats (like camelCase) to their canonical
// snake_case representation used in the YAML struct tags. This allows for flexible
// and case-insensitive key naming in the config.yaml file.
var yamlKeyAliases = map[string]string{
	// Top-level keys
	"loglevel":                 "log_level",
	"pollinginterval":          "poll_interval",
	"cleanupinterval":          "cleanup_interval",
	"lineending":               "line_ending",
	"idletimeout":              "idle_timeout",
	"outoordertolerance":       "out_of_order_tolerance",
	"timestampformat":          "timestamp_format",
	"logformatregex":           "log_format_regex",
	"defaultblockduration":     "default_block_duration",
	"blockermaxretries":        "blocker_max_retries",
	"blockeraddresses":         "blocker_addresses",
	"blockerretrydelay":        "blocker_retry_delay",
	"blockerdialtimeout":       "blocker_dial_timeout",
	"blockercommandqueuesize":  "blocker_command_queue_size",
	"blockercommandspersecond": "blocker_commands_per_second",
	"unblockongoodactor":       "unblock_on_good_actor",
	"unblockcooldown":          "unblock_cooldown",

	"durationtables": "duration_tables",

	// Chain-level keys
	"blockduration": "block_duration",
	"matchkey":      "match_key",
	"onmatch":       "on_match",

	// Step-level keys
	"fieldmatches":        "field_matches",
	"maxdelay":            "max_delay",
	"mindelay":            "min_delay",
	"mintimesincelasthit": "min_time_since_last_hit",

	// Field name aliases (previously in fieldNameMapping)
	// These map lowercase/snake_case field names to their canonical PascalCase form.
	"ip":          "IP",
	"path":        "Path",
	"method":      "Method",
	"protocol":    "Protocol",
	"useragent":   "UserAgent",
	"user_agent":  "UserAgent",
	"referrer":    "Referrer",
	"statuscode":  "StatusCode",
	"status_code": "StatusCode",
	"size":        "Size",
	"vhost":       "VHost",
}

// logConfigurationSummary logs the key-value pairs of the current application configuration.
// This is useful for visibility on startup and after a configuration reload.
func logConfigurationSummary(p *Processor) {
	p.ConfigMutex.RLock()
	config := p.Config
	logRegex := p.LogRegex
	currentLogLevel := logging.GetLogLevel().String()
	p.ConfigMutex.RUnlock()

	p.LogFunc(logging.LevelDebug, "CONFIG", "Loaded configuration:")

	// Handle special cases first
	p.LogFunc(logging.LevelDebug, "CONFIG", "  - line_ending: %s", config.LineEnding)
	p.LogFunc(logging.LevelDebug, "CONFIG", "  - log_level: %s", currentLogLevel)

	// Use reflection to iterate over tagged fields in AppConfig
	val := reflect.ValueOf(*config)
	typ := val.Type()

	for i := 0; i < val.NumField(); i++ {
		structField := typ.Field(i)
		tag := structField.Tag.Get("summary")
		if tag == "" {
			continue // Skip fields without the summary tag
		}

		fieldValue := val.Field(i).Interface()
		p.LogFunc(logging.LevelDebug, "CONFIG", "  - %s: %v", tag, fieldValue)
	}

	// Only show timestamp format if it's not the default.
	if config.TimestampFormat != AccessLogTimeFormat {
		p.LogFunc(logging.LevelDebug, "CONFIG", "  - timestamp_format: custom")
	}

	// Only show log format regex if it's custom.
	if logRegex != nil {
		p.LogFunc(logging.LevelDebug, "CONFIG", "  - log_format_regex: custom")
	}
}

// logChainDetails logs details for a given list of chains, one per line.
func logChainDetails(p *Processor, chains []BehavioralChain, header string) {
	p.LogFunc(logging.LevelDebug, "CONFIG", "%s (%d total)", header, len(chains))
	for _, chain := range chains {
		details := fmt.Sprintf("Name: '%s', Action: %s, Steps: %d, MatchKey: %s", chain.Name, chain.Action, len(chain.Steps), chain.MatchKey)
		// Always show block duration for clarity, indicating if it's a default.
		if chain.BlockDurationStr != "" {
			details += fmt.Sprintf(", BlockDuration: %s", chain.BlockDurationStr)
		} else if chain.UsesDefaultBlockDuration && chain.BlockDuration > 0 {
			// Fallback for default duration which doesn't have an original string from a chain
			details += fmt.Sprintf(", BlockDuration: default(%s)", chain.BlockDuration)
		}
		p.LogFunc(logging.LevelDebug, "CONFIG", "  - %s", details)
	}
}

// areChainsSemanticallyEqual compares two BehavioralChain structs for logical equality,
// ignoring non-comparable fields like function pointers (Matchers).
func areChainsSemanticallyEqual(a, b BehavioralChain) bool {
	// Compare simple fields first, including the original duration string.
	if a.Name != b.Name || a.Action != b.Action ||
		a.BlockDuration != b.BlockDuration || a.MatchKey != b.MatchKey ||
		a.UsesDefaultBlockDuration != b.UsesDefaultBlockDuration || // Check if default usage has changed
		a.BlockDurationStr != b.BlockDurationStr {
		return false
	}

	// Compare steps.
	if len(a.Steps) != len(b.Steps) {
		return false
	}

	// The most reliable way to check for changes is to compare the original YAML step definitions.
	// This correctly detects changes in field_matches, which is not possible with the compiled StepDef.
	return reflect.DeepEqual(a.StepsYAML, b.StepsYAML)
}

// compareConfigsByTag uses reflection to compare fields of two config structs
// that are marked with the `config:"compare"` tag. It returns true if any
// of the tagged fields have different values.
func compareConfigsByTag(oldCfg AppConfig, newCfg LoadedConfig) bool {
	newVal := reflect.ValueOf(newCfg)
	oldVal := reflect.ValueOf(oldCfg)
	newType := newVal.Type()

	for i := 0; i < newVal.NumField(); i++ {
		field := newType.Field(i)
		tag := field.Tag.Get("config")

		// Only compare fields that have the "compare" tag.
		if tag != "compare" {
			continue
		}

		fieldName := field.Name
		newFieldValue := newVal.FieldByName(fieldName)
		oldFieldValue := oldVal.FieldByName(fieldName)

		if !oldFieldValue.IsValid() {
			// This should not happen if AppConfig and LoadedConfig are kept in sync.
			continue
		}

		// Use DeepEqual for slices and maps, otherwise compare interfaces.
		// Note: LogLevel is a special case handled outside this function.
		if newFieldValue.Kind() == reflect.Slice || newFieldValue.Kind() == reflect.Map {
			if !reflect.DeepEqual(newFieldValue.Interface(), oldFieldValue.Interface()) {
				return true // Found a difference
			}
		} else {
			if newFieldValue.Interface() != oldFieldValue.Interface() {
				return true // Found a difference
			}
		}
	}

	return false // No differences found in tagged fields.
}

// --- New Matcher Compilation Logic ---

// fieldMatcher is a function type that represents a compiled matching rule.
// It takes a LogEntry and returns true if the entry satisfies the rule.
type fieldMatcher func(entry *LogEntry) bool

// compileMatchers parses the raw `field_matches` interface from YAML into a slice of efficient matcher functions.
func compileMatchers(chainName string, stepIndex int, fieldMatches map[string]interface{}, fileDeps *[]string, configDir string) ([]fieldMatcher, error) {
	var matchers []fieldMatcher
	for field, value := range fieldMatches {
		// Field names are already normalized by normalizeYAMLKeys before this function is called.
		matcher, err := compileSingleMatcher(chainName, stepIndex, field, value, fileDeps, configDir)
		if err != nil {
			return nil, err // Propagate error up
		}
		matchers = append(matchers, matcher)
	}
	return matchers, nil
}

// compileSingleMatcher is a large switch that handles the different value "shapes" (string, int, list, map).
func compileSingleMatcher(chainName string, stepIndex int, field string, value interface{}, fileDeps *[]string, configDir string) (fieldMatcher, error) {
	switch v := value.(type) {
	case string:
		return compileStringMatcher(chainName, stepIndex, field, v, fileDeps, configDir)
	case int:
		return compileIntMatcher(field, v), nil
	case []interface{}:
		return compileListMatcher(chainName, stepIndex, field, v, fileDeps, configDir)
	case map[string]interface{}:
		// Generalize object matcher for 'not' on any field, and ranges on StatusCode.
		return compileObjectMatcher(chainName, stepIndex, field, v, fileDeps, configDir)
	default:
		return nil, fmt.Errorf("chain '%s', step %d, field '%s': unsupported value type '%T'", chainName, stepIndex+1, field, v)
	}
}

// readLinesFromFile is a helper to read a file into a slice of strings, ignoring comments and empty lines.
func readLinesFromFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Ignore empty lines and lines that start with '#'
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

// compileStringMatcher handles string values, which can be exact, regex, glob, or status code patterns.
func compileStringMatcher(chainName string, stepIndex int, field, value string, fileDeps *[]string, configDir string) (fieldMatcher, error) {
	if strings.HasPrefix(value, "exact:") {
		literalValue := strings.TrimPrefix(value, "exact:")
		return func(entry *LogEntry) bool {
			fieldVal := GetMatchValueIfType(field, entry, StringField)
			if fieldVal == nil {
				return false
			}
			return fieldVal.(string) == literalValue
		}, nil
	}

	// If the value is exactly "file:" or "regex:", treat it as a literal string match.
	// This is more intuitive than requiring "exact:file:".
	if value == "file:" || value == "regex:" || value == "cidr:" {
		// Fall through to the default exact string match at the end of the function.
	} else if strings.HasPrefix(value, "file:") {
		relativeFilePath := strings.TrimPrefix(value, "file:")
		var absoluteFilePath string
		if filepath.IsAbs(relativeFilePath) {
			absoluteFilePath = relativeFilePath
		} else {
			absoluteFilePath = filepath.Join(configDir, relativeFilePath)
		}

		*fileDeps = append(*fileDeps, absoluteFilePath) // Track file for watching
		lines, err := readLinesFromFile(absoluteFilePath)
		if err != nil {
			// Log a warning but do not fail the entire config load.
			// Treat the file as empty, effectively disabling this part of the rule.
			logging.LogOutput(logging.LevelWarning, "CONFIG_WARN", "Chain '%s', step %d, field '%s': failed to read file matcher '%s', it will be treated as empty: %v", chainName, stepIndex+1, field, absoluteFilePath, err)
			// Return a matcher for an empty list, which will never match. Do not return an error.
			lines = []string{}
		}
		// Convert []string to []interface{} to reuse compileListMatcher
		interfaceSlice := make([]interface{}, len(lines))
		for i, v := range lines {
			interfaceSlice[i] = v
		}
		return compileListMatcher(chainName, stepIndex, field, interfaceSlice, fileDeps, configDir)
	} else if strings.HasPrefix(value, "regex:") {
		pattern := strings.TrimPrefix(value, "regex:")
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("chain '%s', step %d, field '%s': invalid regex '%s': %w", chainName, stepIndex+1, field, pattern, err)
		}
		return func(entry *LogEntry) bool {
			fieldVal := GetMatchValueIfType(field, entry, StringField)
			if fieldVal == nil {
				return false
			}
			return re.MatchString(fieldVal.(string))
		}, nil
	} else if strings.HasPrefix(value, "cidr:") {
		if field != "IP" {
			return nil, fmt.Errorf("chain '%s', step %d, field '%s': 'cidr:' matcher is only supported for the 'IP' field", chainName, stepIndex+1, field)
		}
		cidrStr := strings.TrimPrefix(value, "cidr:")
		_, ipNet, err := net.ParseCIDR(cidrStr)
		if err != nil {
			return nil, fmt.Errorf("chain '%s', step %d, field '%s': invalid CIDR '%s' for 'cidr:' matcher: %w", chainName, stepIndex+1, field, cidrStr, err)
		}
		return func(entry *LogEntry) bool {
			fieldVal := GetMatchValueIfType(field, entry, StringField)
			if fieldVal == nil {
				return false
			}
			ipStr := fieldVal.(string)
			ip := net.ParseIP(ipStr)
			if ip == nil {
				return false
			}
			return ipNet.Contains(ip)
		}, nil
	}

	// Special handling for status code patterns like "4XX"
	if field == "StatusCode" && strings.Contains(strings.ToUpper(value), "X") {
		xIndex := strings.Index(strings.ToUpper(value), "X")
		if xIndex > 0 {
			prefix := value[:xIndex]
			// Ensure the prefix is numeric before creating the matcher.
			if _, err := strconv.Atoi(prefix); err == nil {
				return func(entry *LogEntry) bool {
					fieldVal := GetMatchValueIfType(field, entry, IntField)
					if fieldVal == nil {
						return false
					}
					// Convert the integer to a string for the prefix check.
					return strings.HasPrefix(strconv.Itoa(fieldVal.(int)), prefix)
				}, nil
			}
		}
	}

	// Default for string is exact match
	return func(entry *LogEntry) bool {
		fieldVal := GetMatchValueIfType(field, entry, StringField)
		if fieldVal == nil {
			return false
		}
		return fieldVal.(string) == value
	}, nil
}

// compileIntMatcher handles exact integer matches.
func compileIntMatcher(field string, value int) fieldMatcher {
	return func(entry *LogEntry) bool {
		fieldVal := GetMatchValueIfType(field, entry, IntField)
		if fieldVal == nil {
			return false
		}
		return fieldVal.(int) == value
	}
}

// compileListMatcher handles lists, creating an OR condition over its items.
func compileListMatcher(chainName string, stepIndex int, field string, values []interface{}, fileDeps *[]string, configDir string) (fieldMatcher, error) {
	var subMatchers []fieldMatcher
	for _, item := range values {
		matcher, err := compileSingleMatcher(chainName, stepIndex, field, item, fileDeps, configDir)
		if err != nil {
			return nil, err // Error in a sub-matcher
		}
		subMatchers = append(subMatchers, matcher)
	}

	return func(entry *LogEntry) bool {
		for _, matcher := range subMatchers {
			if matcher(entry) {
				return true // OR logic: one match is enough
			}
		}
		return false
	}, nil
}

// compileObjectMatcher handles map values, creating an AND condition for its sub-matchers.
func compileObjectMatcher(chainName string, stepIndex int, field string, obj map[string]interface{}, fileDeps *[]string, configDir string) (fieldMatcher, error) {
	var subMatchers []fieldMatcher

	for key, val := range obj {
		var matcher fieldMatcher
		var err error

		switch key {
		case "gt", "gte", "lt", "lte":
			matcher, err = compileRangeMatcher(chainName, stepIndex, field, key, val)
		case "not":
			matcher, err = compileNotMatcher(chainName, stepIndex, field, val, fileDeps, configDir)
		default:
			return nil, fmt.Errorf("chain '%s', step %d, field '%s': unknown operator '%s' in object matcher", chainName, stepIndex+1, field, key)
		}
		if err != nil {
			return nil, err
		}

		subMatchers = append(subMatchers, matcher)
	}

	if len(subMatchers) == 0 {
		return nil, errors.New("object matcher must not be empty")
	}

	return func(entry *LogEntry) bool {
		for _, matcher := range subMatchers {
			if !matcher(entry) {
				return false // AND logic: one failure means total failure
			}
		}
		return true
	}, nil
}

// compileRangeMatcher handles numeric range operators (gt, gte, lt, lte).
func compileRangeMatcher(chainName string, stepIndex int, field, op string, value interface{}) (fieldMatcher, error) {
	num, ok := value.(int)
	if !ok {
		return nil, fmt.Errorf("chain '%s', step %d, field '%s': value for '%s' must be an integer, got %T", chainName, stepIndex+1, field, op, value)
	}

	// Validate that the field is a known numeric field at compile time.
	// We can do this by calling GetMatchValue with a nil entry and checking the returned type.
	_, fieldType, _ := GetMatchValue(field, nil)
	if fieldType != IntField {
		// This error message is now more generic, which is good.
		return nil, fmt.Errorf("chain '%s', step %d: operator '%s' is only supported for numeric fields, not '%s'", chainName, stepIndex+1, op, field)
	}

	// Return a function that performs the comparison on the generic numeric field.
	return func(entry *LogEntry) bool {
		fieldVal := GetMatchValueIfType(field, entry, IntField)
		if fieldVal == nil {
			return false
		}
		intVal := fieldVal.(int)

		switch op {
		case "gt":
			return intVal > num
		case "gte":
			return intVal >= num
		case "lt":
			return intVal < num
		case "lte":
			return intVal <= num
		}
		return false // Should be unreachable
	}, nil
}

// compileNotMatcher handles the 'not' operator.
func compileNotMatcher(chainName string, stepIndex int, field string, value interface{}, fileDeps *[]string, configDir string) (fieldMatcher, error) {
	// The value of 'not' can be a single item or a list of items.
	// We can reuse the existing list and single matcher compilers.
	var subMatcher fieldMatcher
	var err error

	if values, ok := value.([]interface{}); ok {
		// If it's a list, compile it as an OR-matcher.
		subMatcher, err = compileListMatcher(chainName, stepIndex, field, values, fileDeps, configDir)
	} else {
		// Otherwise, compile it as a single matcher.
		subMatcher, err = compileSingleMatcher(chainName, stepIndex, field, value, fileDeps, configDir)
	}

	if err != nil {
		return nil, err
	}

	// The final matcher is the inverse of the sub-matcher.
	return func(entry *LogEntry) bool {
		return !subMatcher(entry)
	}, nil
}

// normalizeYAMLKeys recursively traverses a yaml.Node tree and converts all
// mapping keys to lowercase. This allows for case-insensitive YAML configuration.
func normalizeYAMLKeys(node *yaml.Node) {
	switch node.Kind {
	case yaml.DocumentNode:
		// The root node is a document node, its content is the actual root element.
		for _, child := range node.Content {
			normalizeYAMLKeys(child)
		}
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			if keyNode.Kind == yaml.ScalarNode {
				// Normalize to lowercase, then check for a canonical alias.
				lowerKey := strings.ToLower(keyNode.Value)
				if canonical, ok := yamlKeyAliases[lowerKey]; ok {
					keyNode.Value = canonical
				} else {
					keyNode.Value = lowerKey
				}
			}
			// Recursively normalize keys in the value node
			normalizeYAMLKeys(node.Content[i+1])
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			normalizeYAMLKeys(child)
		}
	}
}

// LoadConfigFromYAML reads, parses, and pre-compiles regexes for the chains.
func LoadConfigFromYAML(configPath string) (*LoadedConfig, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read YAML file %s: %w", configPath, err)
	}
	configDir := filepath.Dir(configPath)

	// 1. Unmarshal into a generic yaml.Node to preprocess keys.
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("failed to unmarshal YAML: %w", err)
	}

	// 2. Normalize all keys to lowercase for case-insensitive configuration.
	normalizeYAMLKeys(&root)

	// 3. Marshal the normalized node back to a byte slice.
	var normalizedYAML bytes.Buffer
	encoder := yaml.NewEncoder(&normalizedYAML)
	encoder.SetIndent(2) // Optional: keeps the marshaled YAML readable
	if err := encoder.Encode(&root); err != nil {
		return nil, fmt.Errorf("failed to re-marshal normalized YAML: %w", err)
	}

	// 4. Decode the normalized YAML into the config struct with strict checking.
	var config ChainConfig
	decoder := yaml.NewDecoder(&normalizedYAML)
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("failed to strictly unmarshal YAML (unknown field found): %w", err)
	}
	// ---------------------------------------------------------------------------------

	// Define the supported versions for this application code.
	if config.Version == "" {
		// Enforce that the 'version' field must be present.
		return nil, fmt.Errorf("configuration file is missing the required 'version' field")
	}

	// Check if the version is supported.
	isSupported := false
	for _, v := range SupportedConfigVersions {
		if config.Version == v {
			isSupported = true
			break // Found a supported version
		}
	}

	if !isSupported {
		// Report an error showing the unsupported version and the list of supported ones.
		supportedList := strings.Join(SupportedConfigVersions, ", ")
		return nil, fmt.Errorf(
			"configuration version mismatch: got '%s', this application supports: %s",
			config.Version,
			supportedList,
		)
	}

	// --- PARSE GLOBAL SETTINGS ---
	var pollingInterval, cleanupInterval, idleTimeout, outOfOrderTolerance, eofPollingDelay time.Duration

	// Set defaults for global settings
	logLevelStr := DefaultLogLevel
	pollingIntervalStr := DefaultPollingInterval
	cleanupIntervalStr := DefaultCleanupInterval
	eofPollingDelayStr := DefaultEOFPollingDelay
	idleTimeoutStr := DefaultIdleTimeout
	lineEndingStr := DefaultLineEnding
	outOfOrderToleranceStr := DefaultOutOfOrderTolerance

	// Override defaults with values from YAML if they exist
	if config.LogLevel != "" {
		logLevelStr = config.LogLevel
	}
	if config.PollingInterval != "" {
		pollingIntervalStr = config.PollingInterval
	}
	if config.CleanupInterval != "" {
		cleanupIntervalStr = config.CleanupInterval
	}
	if config.EOFPollingDelay != "" {
		eofPollingDelayStr = config.EOFPollingDelay
	}
	if config.IdleTimeout != "" {
		idleTimeoutStr = config.IdleTimeout
	}
	if config.LineEnding != "" {
		lineEndingStr = config.LineEnding
	}
	if config.OutOfOrderTolerance != "" {
		outOfOrderToleranceStr = config.OutOfOrderTolerance
	}

	// Parse unblock settings
	unblockCooldownStr := DefaultUnblockCooldown
	if config.UnblockCooldown != "" {
		unblockCooldownStr = config.UnblockCooldown
	}

	// Validate line_ending
	switch lineEndingStr {
	case "lf", "crlf", "cr":
		// valid
	default:
		return nil, fmt.Errorf("invalid line_ending value: '%s'. Must be one of 'lf', 'crlf', 'cr'", lineEndingStr)
	}

	// Parse durations
	pollingInterval, err = time.ParseDuration(pollingIntervalStr)
	if err != nil {
		return nil, fmt.Errorf("invalid poll_interval format: %w", err)
	}
	cleanupInterval, err = time.ParseDuration(cleanupIntervalStr)
	if err != nil {
		return nil, fmt.Errorf("invalid cleanup_interval format: %w", err)
	}
	eofPollingDelay, err = time.ParseDuration(eofPollingDelayStr)
	if err != nil {
		return nil, fmt.Errorf("invalid eof_polling_delay format: %w", err)
	}
	idleTimeout, err = time.ParseDuration(idleTimeoutStr)
	if err != nil {
		return nil, fmt.Errorf("invalid idle_timeout format: %w", err)
	}
	outOfOrderTolerance, err = time.ParseDuration(outOfOrderToleranceStr)
	if err != nil {
		return nil, fmt.Errorf("invalid out_of_order_tolerance format: %w", err)
	}
	unblockCooldown, err := time.ParseDuration(unblockCooldownStr)
	if err != nil {
		return nil, fmt.Errorf("invalid unblock_cooldown format: %w", err)
	}

	// Parse custom timestamp format if provided, otherwise use default.
	timestampFormat := AccessLogTimeFormat
	if config.TimestampFormat != "" {
		timestampFormat = config.TimestampFormat
	}

	// Parse custom log format regex if provided
	var customLogRegex *regexp.Regexp
	if config.LogFormatRegex != "" {
		re, err := regexp.Compile(config.LogFormatRegex)
		if err != nil {
			return nil, fmt.Errorf("invalid log_format_regex: %w", err)
		}
		// Validate that the regex has the required named capture groups.
		requiredGroups := []string{"IP", "Timestamp"}
		foundGroups := make(map[string]bool)
		for _, name := range re.SubexpNames() {
			if name != "" {
				foundGroups[name] = true
			}
		}

		for _, required := range requiredGroups {
			if !foundGroups[required] {
				return nil, fmt.Errorf("invalid log_format_regex: missing required named capture group '(?P<%s>...)'", required)
			}
		}

		customLogRegex = re
	}

	// --- PARSE DURATION TABLES ---
	newDurationTables := make(map[time.Duration]string, len(config.DurationTables))
	longestDuration := 0 * time.Second
	newFallbackName := ""

	for durationStr, tableName := range config.DurationTables {
		duration, err := time.ParseDuration(durationStr)
		if err != nil {
			return nil, fmt.Errorf("invalid duration '%s' in 'duration_tables': %w", durationStr, err)
		}
		newDurationTables[duration] = tableName

		// Find the longest duration to set the fallback table name
		if duration > longestDuration {
			longestDuration = duration
			newFallbackName = tableName
		}
	}

	// --- PARSE BLOCKER SETTINGS ---
	var blockerMaxRetries int
	var blockerRetryDelay, blockerDialTimeout time.Duration
	var blockerCommandQueueSize, blockerCommandsPerSecond int

	if config.BlockerMaxRetries > 0 {
		blockerMaxRetries = config.BlockerMaxRetries
	} else {
		blockerMaxRetries = DefaultBlockerMaxRetries
	}

	if config.BlockerRetryDelay != "" {
		blockerRetryDelay, err = time.ParseDuration(config.BlockerRetryDelay)
		if err != nil {
			return nil, fmt.Errorf("invalid blocker_retry_delay: %w", err)
		}
	} else {
		blockerRetryDelay = DefaultBlockerRetryDelay
	}

	if config.BlockerDialTimeout != "" {
		blockerDialTimeout, err = time.ParseDuration(config.BlockerDialTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid blocker_dial_timeout: %w", err)
		}
	} else {
		blockerDialTimeout = DefaultBlockerDialTimeout
	}

	if config.BlockerCommandQueueSize > 0 {
		blockerCommandQueueSize = config.BlockerCommandQueueSize
	} else {
		blockerCommandQueueSize = DefaultBlockerCommandQueueSize
	}

	if config.BlockerCommandsPerSecond > 0 {
		blockerCommandsPerSecond = config.BlockerCommandsPerSecond
	} else {
		blockerCommandsPerSecond = DefaultBlockerCommandsPerSecond
	}

	// --- PARSE CHAINS ---
	newChains := make([]BehavioralChain, 0)

	// Pre-parse the default block duration once.
	var defaultBlockDuration time.Duration
	if config.DefaultBlockDuration != "" {
		var err error
		defaultBlockDuration, err = utils.ParseDuration(config.DefaultBlockDuration)
		if err != nil {
			return nil, fmt.Errorf("invalid block_duration format for default_block_duration: %w", err)
		}
	}

	// --- PARSE FILE DEPENDENCIES ---
	fileDependencies := []string{}

	for _, yamlChain := range config.Chains {
		// Check if the chain is explicitly disabled via the action field.
		if strings.HasPrefix(yamlChain.Action, "!") {
			// Log at debug level for visibility without cluttering the default output.
			logging.LogOutput(logging.LevelDebug, "CONFIG_SKIP", "Skipping disabled chain '%s' (action: %s)", yamlChain.Name, yamlChain.Action)
			continue // Skip this chain entirely.
		}

		var blockDuration time.Duration
		usesDefault := false
		blockDurationStr := yamlChain.BlockDuration // Keep original string for logging
		if yamlChain.BlockDuration != "" {
			var err error
			blockDuration, err = utils.ParseDuration(yamlChain.BlockDuration)
			if err != nil {
				return nil, fmt.Errorf("chain '%s': invalid block_duration format: %w", yamlChain.Name, err)
			}
		} else {
			// If the chain's duration is not set, it will use the default.
			// We assign it here so that logging and comparison are accurate.
			blockDuration = defaultBlockDuration
			blockDurationStr = config.DefaultBlockDuration
			// Mark that the default is being used, regardless of the action.
			usesDefault = true
		}

		// 4. Enforce that 'block' actions must have a non-zero duration.
		if yamlChain.Action == "block" && blockDuration == 0 {
			// This is a non-fatal warning. The chain will not be loaded.
			logging.LogOutput(logging.LevelWarning, "CONFIG_WARN", "chain '%s' has action 'block' but block_duration is missing or zero and no default is set. This chain will be skipped.", yamlChain.Name)
			// Skip adding this invalid chain to the list of new chains.
			continue
		}

		// Validate block durations against duration tables.
		if yamlChain.Action == "block" && blockDuration > 0 {
			if len(newDurationTables) == 0 {
				// This chain needs a duration table, but none are configured.
				logging.LogOutput(logging.LevelWarning, "CONFIG_WARN", "chain '%s' has a block_duration of '%s', but no 'duration_tables' are configured. Block actions for this chain may fail if not in dry-run mode.", yamlChain.Name, blockDurationStr)
			} else if _, ok := newDurationTables[blockDuration]; !ok {
				// Duration tables are configured, but this specific duration is missing.
				logging.LogOutput(logging.LevelWarning, "CONFIG_WARN", "chain '%s' has a block_duration of '%s' which is not defined in 'duration_tables'. Block actions for this chain may fail if not in dry-run mode.", yamlChain.Name, blockDurationStr)
			}
		}

		// 2. Validate Match Key
		if yamlChain.MatchKey == "" {
			return nil, fmt.Errorf("chain '%s': match_key cannot be empty", yamlChain.Name)
		}

		runtimeChain := BehavioralChain{
			Name:                     yamlChain.Name,
			Action:                   yamlChain.Action,
			BlockDuration:            blockDuration,
			BlockDurationStr:         blockDurationStr,
			UsesDefaultBlockDuration: usesDefault,
			MatchKey:                 yamlChain.MatchKey,
			OnMatch:                  yamlChain.OnMatch,
			StepsYAML:                yamlChain.Steps, // Store the original YAML steps for comparison
		}

		// Initialize a counter for this chain in the metrics map.
		runtimeChain.MetricsCounter = new(atomic.Int64)
		runtimeChain.MetricsResetCounter = new(atomic.Int64)
		runtimeChain.MetricsHitsCounter = new(atomic.Int64)

		// 3. Process Steps
		for i, yamlStep := range yamlChain.Steps {
			runtimeStep := StepDef{
				Order: i + 1,
				// FieldMatches is no longer stored directly in the runtime step.
				// It's compiled into Matchers.
			}

			// Parse delays
			if yamlStep.MaxDelay != "" {
				runtimeStep.MaxDelayDuration, err = time.ParseDuration(yamlStep.MaxDelay)
				if err != nil {
					return nil, fmt.Errorf("chain '%s', step %d: invalid max_delay: %w", yamlChain.Name, runtimeStep.Order, err)
				}
			}
			if yamlStep.MinDelay != "" {
				runtimeStep.MinDelayDuration, err = time.ParseDuration(yamlStep.MinDelay)
				if err != nil {
					return nil, fmt.Errorf("chain '%s', step %d: invalid min_delay: %w", yamlChain.Name, runtimeStep.Order, err)
				}
			}
			if i == 0 && yamlStep.MinTimeSinceLastHit != "" { // Only parse for the first step
				runtimeStep.MinTimeSinceLastHit, err = time.ParseDuration(yamlStep.MinTimeSinceLastHit)
				if err != nil {
					return nil, fmt.Errorf("chain '%s', step %d: invalid min_time_since_last_hit: %w", yamlChain.Name, runtimeStep.Order, err)
				}
			}

			// Compile the new flexible matchers
			runtimeStep.Matchers, err = compileMatchers(yamlChain.Name, i, yamlStep.FieldMatches, &fileDependencies, configDir)
			if err != nil {
				return nil, err // Error from compilation
			}
			runtimeChain.Steps = append(runtimeChain.Steps, runtimeStep)
		}
		newChains = append(newChains, runtimeChain)
	}

	// --- PARSE GOOD ACTORS ---
	var newGoodActors []GoodActorDef
	for _, goodActorMap := range config.GoodActors {
		nameVal, ok := goodActorMap["name"]
		if !ok {
			return nil, fmt.Errorf("a 'good_actors' entry is missing the required 'name' field")
		}
		name, ok := nameVal.(string)
		if !ok {
			return nil, fmt.Errorf("a 'good_actors' entry has a 'name' field that is not a string")
		}

		def := GoodActorDef{Name: name}

		// Iterate over the definition map to find IP and UserAgent keys case-insensitively.
		for key, value := range goodActorMap {
			switch strings.ToLower(key) {
			case "ip":
				var ipList []interface{}
				if list, isList := value.([]interface{}); isList {
					ipList = list
				} else {
					ipList = []interface{}{value}
				}
				matcher, err := compileListMatcher(fmt.Sprintf("good_actor '%s'", name), 0, "IP", ipList, &fileDependencies, configDir)
				if err != nil {
					return nil, err
				}
				def.IPMatchers = []fieldMatcher{matcher}
			case "useragent", "user_agent":
				var uaList []interface{}
				if list, isList := value.([]interface{}); isList {
					uaList = list
				} else {
					uaList = []interface{}{value}
				}
				matcher, err := compileListMatcher(fmt.Sprintf("good_actor '%s'", name), 0, "UserAgent", uaList, &fileDependencies, configDir)
				if err != nil {
					return nil, err
				}
				def.UAMatchers = []fieldMatcher{matcher}
			}
		}

		if len(def.IPMatchers) > 0 || len(def.UAMatchers) > 0 {
			newGoodActors = append(newGoodActors, def)
		}
	}

	// Find the maximum min_time_since_last_hit duration across all chains for cleanup optimization.
	var maxTimeSinceLastHit time.Duration
	for _, chain := range newChains {
		if len(chain.Steps) > 0 && chain.Steps[0].MinTimeSinceLastHit > maxTimeSinceLastHit {
			maxTimeSinceLastHit = chain.Steps[0].MinTimeSinceLastHit
		}
	}

	return &LoadedConfig{
		GoodActors:               newGoodActors,
		BlockTableNameFallback:   newFallbackName,
		Chains:                   newChains,
		DefaultBlockDuration:     defaultBlockDuration,
		CleanupInterval:          cleanupInterval,
		EOFPollingDelay:          eofPollingDelay,
		DurationToTableName:      newDurationTables,
		FileDependencies:         fileDependencies,
		BlockerAddresses:         config.BlockerAddresses,
		BlockerDialTimeout:       blockerDialTimeout,
		BlockerMaxRetries:        blockerMaxRetries,
		BlockerRetryDelay:        blockerRetryDelay,
		BlockerCommandQueueSize:  blockerCommandQueueSize,
		BlockerCommandsPerSecond: blockerCommandsPerSecond,
		IdleTimeout:              idleTimeout,
		LogLevel:                 logLevelStr,
		LineEnding:               lineEndingStr,
		LogFormatRegex:           customLogRegex,
		MaxTimeSinceLastHit:      maxTimeSinceLastHit,
		OutOfOrderTolerance:      outOfOrderTolerance,
		PollingInterval:          pollingInterval,
		TimestampFormat:          timestampFormat,
		UnblockOnGoodActor:       config.UnblockOnGoodActor,
		UnblockCooldown:          unblockCooldown,
	}, nil
}

// reloadConfiguration contains the logic to reload configuration, compare for changes,
// and update the processor state. It is designed to be called by either the
// file watcher or the signal reloader.
func reloadConfiguration(p *Processor) { //nolint:cyclop
	p.ConfigMutex.RLock()
	oldChains := p.Chains
	// Create a deep copy of the config to compare against after the reload.
	// A shallow copy (*p.Config) would lead to a race condition as both
	// old and new configs would share the same RWMutex.
	oldConfig := p.Config.Clone()
	oldLogRegex := p.LogRegex
	p.ConfigMutex.RUnlock()

	loadedCfg, err := LoadConfigFromYAML(p.ConfigPath)
	if err != nil {
		p.LogFunc(logging.LevelError, "LOAD_ERROR", "Failed to reload configuration: %v", err)
		return // The deferred signal will still fire.
	}

	// Create a new AppConfig from the loaded configuration.
	newAppConfig := &AppConfig{
		GoodActors:               loadedCfg.GoodActors,
		BlockTableNameFallback:   loadedCfg.BlockTableNameFallback,
		CleanupInterval:          loadedCfg.CleanupInterval,
		DefaultBlockDuration:     loadedCfg.DefaultBlockDuration,
		DurationToTableName:      loadedCfg.DurationToTableName,
		EOFPollingDelay:          loadedCfg.EOFPollingDelay,
		FileDependencies:         loadedCfg.FileDependencies,
		BlockerAddresses:         loadedCfg.BlockerAddresses,
		BlockerDialTimeout:       loadedCfg.BlockerDialTimeout,
		BlockerMaxRetries:        loadedCfg.BlockerMaxRetries,
		BlockerRetryDelay:        loadedCfg.BlockerRetryDelay,
		BlockerCommandQueueSize:  loadedCfg.BlockerCommandQueueSize,
		BlockerCommandsPerSecond: loadedCfg.BlockerCommandsPerSecond,
		IdleTimeout:              loadedCfg.IdleTimeout,
		LineEnding:               loadedCfg.LineEnding,
		LastModTime:              time.Now(), // Use time.Now() to avoid race conditions
		MaxTimeSinceLastHit:      loadedCfg.MaxTimeSinceLastHit,
		OutOfOrderTolerance:      loadedCfg.OutOfOrderTolerance,
		PollingInterval:          loadedCfg.PollingInterval,
		TimestampFormat:          loadedCfg.TimestampFormat,
		UnblockOnGoodActor:       loadedCfg.UnblockOnGoodActor,
		UnblockCooldown:          loadedCfg.UnblockCooldown,

		StatFunc: oldConfig.StatFunc, // Preserve mockable functions
	}

	// Update the processor's state with the new config.
	p.ConfigMutex.Lock()
	p.Chains = loadedCfg.Chains
	p.Config = newAppConfig // Atomically swap the config pointer.
	p.LogRegex = loadedCfg.LogFormatRegex
	initializeMetrics(p, loadedCfg)

	logging.SetLogLevel(loadedCfg.LogLevel)
	p.ConfigMutex.Unlock()

	// --- Compare and log general config changes ---
	configChanged := compareConfigsByTag(oldConfig, *loadedCfg) ||
		loadedCfg.LogLevel != logging.GetLogLevel().String() ||
		(oldLogRegex != nil) != (loadedCfg.LogFormatRegex != nil)

	if configChanged {
		p.LogFunc(logging.LevelInfo, "CONFIG", "General configuration settings have been updated.")
		logConfigurationSummary(p)
	}

	// --- Compare and log chain differences ---
	oldChainsMap := make(map[string]BehavioralChain)
	for _, chain := range oldChains {
		oldChainsMap[chain.Name] = chain
	}
	newChainsMap := make(map[string]BehavioralChain)
	for _, chain := range loadedCfg.Chains {
		newChainsMap[chain.Name] = chain
	}

	var added, removed, modified []BehavioralChain
	for name, newChain := range newChainsMap {
		if oldChain, exists := oldChainsMap[name]; !exists {
			added = append(added, newChain)
		} else if !areChainsSemanticallyEqual(oldChain, newChain) {
			modified = append(modified, newChain)
		}
	}
	for name, oldChain := range oldChainsMap {
		if _, exists := newChainsMap[name]; !exists {
			removed = append(removed, oldChain)
		}
	}

	if len(added) > 0 {
		logChainDetails(p, added, "Added chains:")
	}
	if len(modified) > 0 {
		logChainDetails(p, modified, "Modified chains:")
	}
	if len(removed) > 0 {
		logChainDetails(p, removed, "Removed chains:")
	}

}

// initializeMetrics sets up all the metric counters based on the loaded configuration.
// It resets and repopulates the metric maps, making it safe to call on both startup and reload.
func initializeMetrics(p *Processor, loadedCfg *LoadedConfig) {
	// Reset and initialize per-chain metrics.
	p.Metrics.ChainsCompleted = &sync.Map{}
	p.Metrics.ChainsReset = &sync.Map{}
	p.Metrics.ChainsHits = &sync.Map{}
	for _, chain := range p.Chains {
		p.Metrics.ChainsCompleted.Store(chain.Name, chain.MetricsCounter)
		p.Metrics.ChainsReset.Store(chain.Name, chain.MetricsResetCounter)
		p.Metrics.ChainsHits.Store(chain.Name, chain.MetricsHitsCounter)
	}

	// Initialize match key hit counters.
	p.Metrics.MatchKeyHits = &sync.Map{}
	matchKeys := []string{"ip", "ipv4", "ipv6", "ip_ua", "ipv4_ua", "ipv6_ua"}
	for _, key := range matchKeys {
		p.Metrics.MatchKeyHits.Store(key, new(atomic.Int64))
	}

	// Initialize block duration counters.
	p.Metrics.BlockDurations = &sync.Map{}
	for duration := range loadedCfg.DurationToTableName {
		p.Metrics.BlockDurations.Store(duration, new(atomic.Int64))
	}
	if loadedCfg.DefaultBlockDuration > 0 {
		p.Metrics.BlockDurations.Store(loadedCfg.DefaultBlockDuration, new(atomic.Int64))
	}

	// Initialize per-blocker command counters.
	p.Metrics.CmdsPerBlocker = &sync.Map{}
	for _, addr := range loadedCfg.BlockerAddresses {
		p.Metrics.CmdsPerBlocker.Store(addr, new(atomic.Int64))
	}
	// Initialize good actor hit counters.
	p.Metrics.GoodActorHits = &sync.Map{}
	for _, goodActor := range loadedCfg.GoodActors {
		p.Metrics.GoodActorHits.Store(goodActor.Name, new(atomic.Int64))
	}
}

// SignalReloader listens for a specific OS signal to trigger a configuration reload. //nolint:cyclop
func SignalReloader(p *Processor, stop <-chan struct{}, signalCh chan os.Signal) {
	if p.DryRun {
		return
	}

	signalName := strings.ToUpper(p.ReloadOnSignal)
	reloadSignal, ok := signalMap[signalName]
	if !ok {
		p.LogFunc(logging.LevelCritical, "FATAL", "Unsupported signal for reloading: '%s'. Use HUP, USR1, or USR2.", p.ReloadOnSignal)
		// In a real app, you might want to stop the process here.
		// For now, we just stop this goroutine.
		return
	}

	signal.Notify(signalCh, reloadSignal) // Notify on the provided channel

	p.LogFunc(logging.LevelInfo, "RELOAD", "Signal-based config reloading enabled. Send %s signal to reload.", signalName)

	for {
		select {
		case <-stop:
			p.LogFunc(logging.LevelInfo, "RELOAD", "SignalReloader received stop signal. Shutting down.")
			return
		case s := <-signalCh:
			p.LogFunc(logging.LevelInfo, "RELOAD", "Received signal %s. Reloading configuration...", s)
			func() { // Use an anonymous function to scope the defer correctly.
				// Defer the test signal to ensure it's sent whether the reload succeeds or fails.
				if p.TestSignals != nil && p.TestSignals.ReloadDoneSignal != nil {
					defer func() { p.TestSignals.ReloadDoneSignal <- struct{}{} }()
				}
				reloadConfiguration(p)
			}()
		}
	}
}

// ConfigWatcher monitors the YAML config file for modifications and reloads the chains dynamically.
func ConfigWatcher(p *Processor, stop <-chan struct{}) {
	if p.DryRun {
		return
	}

	// Enforce a minimum safe interval.
	pollingInterval := p.Config.PollingInterval
	if pollingInterval < DefaultMinPollingInterval {
		pollingInterval = DefaultMinPollingInterval
	}

	p.LogFunc(logging.LevelDebug, "WATCH", "Starting ConfigWatcher, polling every %v", pollingInterval)
	timer := time.NewTicker(pollingInterval)
	defer timer.Stop()

	// Conditionally include the test channel in the select statement.
	forceCheckCh := make(chan struct{}) // A dummy channel that is never written to.
	if p.TestSignals != nil {
		forceCheckCh = p.TestSignals.ForceCheckSignal
	}

	for {
		select {
		case <-stop:
			p.LogFunc(logging.LevelInfo, "WATCH", "ConfigWatcher received stop signal. Shutting down.")
			return
		case <-forceCheckCh:
			// This case is for testing only, to trigger an immediate check.
			if p.TestSignals != nil { // Double-check for safety, though it should always be true here.
				p.LogFunc(logging.LevelDebug, "WATCH", "Received test signal for immediate reload check.")
			}
		case <-timer.C:
			// Timer fired, continue with polling.
		}

		isChanged := false
		changedFile := ""

		// 1. Check the main YAML file
		fileInfo, err := os.Stat(p.ConfigPath)
		if err != nil {
			p.LogFunc(logging.LevelError, "WATCH_ERROR", "Failed to stat file %s: %v", p.ConfigPath, err)
			continue
		}

		p.ConfigMutex.RLock()
		if fileInfo.ModTime().After(p.Config.LastModTime) {
			isChanged = true
			changedFile = p.ConfigPath
		} else {
			// 2. Check all file dependencies if YAML hasn't changed
			for _, depPath := range p.Config.FileDependencies {
				depInfo, err := os.Stat(depPath)
				if err != nil {
					// Log at debug level if a dependency file is missing, as this might be temporary.
					// A reload will only be triggered if config.yaml itself changes.
					p.LogFunc(logging.LevelDebug, "WATCH_SKIP", "Could not stat dependency file %s (may have been removed): %v", depPath, err)
					continue
				}
				if depInfo.ModTime().After(p.Config.LastModTime) {
					isChanged = true
					changedFile = depPath
					break
				}
			}
		}
		p.ConfigMutex.RUnlock()

		if isChanged {
			p.LogFunc(logging.LevelInfo, "WATCH", "Detected change in '%s'. Attempting reload...", changedFile)
			func() { // Use an anonymous function to scope the defer correctly.
				// Defer the test signal to ensure it's sent whether the reload succeeds or fails.
				if p.TestSignals != nil && p.TestSignals.ReloadDoneSignal != nil {
					defer func() { p.TestSignals.ReloadDoneSignal <- struct{}{} }()
				}
				reloadConfiguration(p)
			}()
		}
	}
}
