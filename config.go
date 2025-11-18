package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
	"bot-detector/internal/types"
	"bot-detector/internal/utils"

	"gopkg.in/yaml.v3"
)

// logStructFields recursively logs the fields of a struct that have a "summary" tag.
func logStructFields(p *Processor, val reflect.Value, typ reflect.Type) {
	for i := 0; i < val.NumField(); i++ {
		structField := typ.Field(i)
		fieldValue := val.Field(i)

		// Recurse into nested structs
		if fieldValue.Kind() == reflect.Struct {
			logStructFields(p, fieldValue, fieldValue.Type())
			continue
		}

		tag := structField.Tag.Get("summary")
		if tag == "" {
			continue // Skip fields without the summary tag
		}

		p.LogFunc(logging.LevelDebug, "CONFIG", "  - %s: %v", tag, fieldValue.Interface())
	}
}

// logConfigurationSummary logs the key-value pairs of the current application configuration.
// This is useful for visibility on startup and after a configuration reload.
func logConfigurationSummary(p *Processor) {
	p.ConfigMutex.RLock()
	config := p.Config
	logRegex := p.LogRegex
	currentLogLevel := logging.GetLogLevel().String()
	p.ConfigMutex.RUnlock()

	if p.configReloaded {
		p.LogFunc(logging.LevelInfo, "CONFIG_RELOAD", "Successfully reloaded main configuration from '%s'", p.ConfigPath)
	} else {
		p.configReloaded = true
		p.LogFunc(logging.LevelInfo, "CONFIG", "Successfully loaded main configuration from '%s'", p.ConfigPath)
	}

	p.LogFunc(logging.LevelDebug, "CONFIG", "Loaded configuration:")

	// Handle special cases first
	p.LogFunc(logging.LevelDebug, "CONFIG", "  - log_level: %s", currentLogLevel)

	// Use reflection to iterate over tagged fields in AppConfig
	val := reflect.ValueOf(*config)
	typ := val.Type()
	logStructFields(p, val, typ)

	// Only show timestamp format if it's not the default.
	if config.Parser.TimestampFormat != AccessLogTimeFormat {
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

// compareConfigs checks if two configurations are semantically different.
func compareConfigs(oldCfg AppConfig, newCfg LoadedConfig) bool {
	// Compare the main configuration sections using DeepEqual.
	// This is simpler and more robust than tag-based reflection for nested structs.
	if !reflect.DeepEqual(oldCfg.Application, newCfg.Application) ||
		!reflect.DeepEqual(oldCfg.Parser, newCfg.Parser) ||
		!reflect.DeepEqual(oldCfg.Checker, newCfg.Checker) ||
		!reflect.DeepEqual(oldCfg.Blockers, newCfg.Blockers) ||
		!reflect.DeepEqual(oldCfg.GoodActors, newCfg.GoodActors) {
		return true
	}

	return false // No differences found.
}

// --- Dependency Graph for Cycle Detection ---

const filePrefix = "file:"

// depGraphNode represents a file in the dependency graph.
type depGraphNode struct {
	Path         string
	Dependencies []*depGraphNode
}

// depGraph represents the dependency graph of configuration files.
type depGraph struct {
	Nodes map[string]*depGraphNode
}

// newDepGraph creates an empty dependency graph.
func newDepGraph() *depGraph {
	return &depGraph{
		Nodes: make(map[string]*depGraphNode),
	}
}

// addNode adds a new file (node) to the graph if it doesn't already exist.
func (g *depGraph) addNode(path string) *depGraphNode {
	if node, exists := g.Nodes[path]; exists {
		return node
	}
	node := &depGraphNode{Path: path}
	g.Nodes[path] = node
	return node
}

// addEdge creates a dependency link from one file to another.
func (g *depGraph) addEdge(fromPath, toPath string) {
	fromNode := g.addNode(fromPath)
	toNode := g.addNode(toPath)
	fromNode.Dependencies = append(fromNode.Dependencies, toNode)
}

// findFileDirectives scans a file for `file:` directives. The main config is parsed as YAML,
// while any included files are treated as plain text.
func findFileDirectives(path string, isMainConfig bool) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		// If a file is missing during dependency scan, it's a critical error.
		if errors.Is(err, syscall.ENOENT) {
			return nil, fmt.Errorf("referenced file not found: %s", path)
		}
		return nil, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			logging.LogOutput(logging.LevelWarning, "FILE_CLOSE_ERROR", "Error closing file %s: %v", path, err)
		}
	}()

	configDir := filepath.Dir(path)
	var dependencies []string

	if isMainConfig {
		// Main config is treated as YAML.
		var findInNode func(node *yaml.Node)
		findInNode = func(node *yaml.Node) {
			switch node.Kind {
			case yaml.DocumentNode:
				for _, child := range node.Content {
					findInNode(child)
				}
			case yaml.MappingNode:
				for i := 1; i < len(node.Content); i += 2 {
					findInNode(node.Content[i])
				}
			case yaml.SequenceNode:
				for _, child := range node.Content {
					findInNode(child)
				}
			case yaml.ScalarNode:
				if strings.HasPrefix(node.Value, filePrefix) {
					relativeDepPath := strings.TrimSpace(strings.TrimPrefix(node.Value, filePrefix))
					if relativeDepPath != "" {
						var absoluteDepPath string
						if filepath.IsAbs(relativeDepPath) {
							absoluteDepPath = relativeDepPath
						} else {
							absoluteDepPath = filepath.Join(configDir, relativeDepPath)
						}
						dependencies = append(dependencies, absoluteDepPath)
					}
				}
			}
		}
		var root yaml.Node
		decoder := yaml.NewDecoder(file)
		if err := decoder.Decode(&root); err == nil {
			findInNode(&root)
		}
		// We ignore YAML decoding errors here; the main loader will report them.
	} else {
		// Included files are treated as plain text.
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			rawLine := scanner.Text()
			// First, check if the line is empty or a comment after trimming.
			// This ensures we don't process empty lines or comments as directives.
			trimmedForCheck := strings.TrimSpace(rawLine)
			if trimmedForCheck == "" || strings.HasPrefix(trimmedForCheck, "#") {
				continue
			}

			// Now, check for the exact "file:" prefix on the raw line.
			// This ensures no spaces before "file:".
			if strings.HasPrefix(rawLine, filePrefix) {
				// Extract the path, preserving its leading/trailing spaces.
				relativeDepPath := strings.TrimPrefix(rawLine, filePrefix) // No TrimSpace here
				if relativeDepPath != "" {
					var absoluteDepPath string
					if filepath.IsAbs(relativeDepPath) {
						absoluteDepPath = relativeDepPath
					} else {
						absoluteDepPath = filepath.Join(configDir, relativeDepPath)
					}
					dependencies = append(dependencies, absoluteDepPath)
				}
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
	}

	logging.LogOutput(logging.LevelInfo, "FILE_SCAN", "Successfully scanned '%s' for file directives. Found %d dependencies.", path, len(dependencies))
	return dependencies, nil
}

// detectCycle performs a DFS traversal to find cycles.
func (g *depGraph) detectCycle(configPath string) error {
	visiting := make(map[string]bool)
	visited := make(map[string]bool)
	// Start the DFS from the first node added to the graph, which is the main config file.
	// This makes cycle detection deterministic, which is important for testing.
	// We find the root node by looking for a node that is not a dependency of any other node.
	// A simpler and sufficient approach for this codebase is to start from the known root config path.
	if path, err := g.dfs(g.Nodes[configPath], visiting, visited, nil); err != nil {
		return fmt.Errorf("cyclic dependency detected: %s", strings.Join(path, " -> "))
	}
	return nil
}

// dfs is the recursive helper for cycle detection.
func (g *depGraph) dfs(node *depGraphNode, visiting, visited map[string]bool, path []string) ([]string, error) {
	visiting[node.Path] = true
	path = append(path, node.Path)

	for _, dep := range node.Dependencies {
		if visiting[dep.Path] {
			// Cycle detected. Append the final node to show the loop and return.
			path = append(path, dep.Path)
			return path, errors.New("cycle found")
		}
		if !visited[dep.Path] {
			// It's crucial to pass a copy of the path slice to the recursive call.
			// This prevents different traversal branches from interfering with each other's path tracking.
			if p, err := g.dfs(dep, visiting, visited, append([]string{}, path...)); err != nil {
				return p, err
			}
		}
	}
	visiting[node.Path] = false
	visited[node.Path] = true
	return nil, nil
}

// --- New Matcher Compilation Logic ---

// MatcherContext holds common parameters needed during matcher compilation.
type MatcherContext struct {
	ChainName          string
	StepIndex          int
	CanonicalFieldName string
	FileDependencies   map[string]*types.FileDependency
	FilePath           string
}

// fieldMatcher is a function type that represents a compiled matching rule.
// It takes a LogEntry and returns true if the entry satisfies the rule.
type fieldMatcher func(entry *LogEntry) bool

// compileMatchers parses the raw `field_matches` interface from YAML into a slice of efficient matcher functions.
func compileMatchers(chainName string, stepIndex int, fieldMatches map[string]interface{}, fileDeps map[string]*types.FileDependency, filePath string) ([]struct {
	Matcher   fieldMatcher
	FieldName string
}, error) {
	var matchers []struct {
		Matcher   fieldMatcher
		FieldName string
	}
	// Create the initial MatcherContext for this chain and step.
	ctx := MatcherContext{
		ChainName:        chainName,
		StepIndex:        stepIndex,
		FileDependencies: fileDeps,
		FilePath:         filePath,
	}

	for field, value := range fieldMatches {
		// Field names are already normalized by normalizeYAMLKeys before this function is called.
		matcher, fieldName, err := compileSingleMatcher(ctx, field, value)
		if err != nil {
			return nil, err // Propagate error up
		}
		matchers = append(matchers, struct {
			Matcher   fieldMatcher
			FieldName string
		}{Matcher: matcher, FieldName: fieldName})
	}
	return matchers, nil
}

// compileSingleMatcher is a large switch that handles the different value "shapes" (string, int, list, map).
func compileSingleMatcher(ctx MatcherContext, field string, value interface{}) (fieldMatcher, string, error) {
	// Convert the incoming fieldName to its canonical PascalCase form for internal matching.
	// This ensures that YAML keys like "ip" map correctly to LogEntry.IPInfo.
	canonicalFieldName, ok := FieldNameCanonicalMap[strings.ToLower(field)]
	if !ok {
		// If not found in the map, assume the fieldName is already canonical or unknown.
		canonicalFieldName = field
	}

	// Create a new context with the canonical field name for subsequent matchers.
	subCtx := ctx
	subCtx.CanonicalFieldName = canonicalFieldName

	var matcher fieldMatcher
	var err error

	switch v := value.(type) {
	case string:
		matcher, err = compileStringMatcher(subCtx, v)
	case int:
		matcher, err = compileIntMatcher(subCtx, v), nil
	case []interface{}:
		matcher, err = compileListMatcher(subCtx, v)
	case map[string]interface{}:
		matcher, err = compileObjectMatcher(subCtx, v)
	default:
		return nil, "", fmt.Errorf("in file '%s': chain '%s', step %d, field '%s': unsupported value type '%T'", ctx.FilePath, ctx.ChainName, ctx.StepIndex+1, field, v)
	}

	if err != nil {
		return nil, "", err
	}
	return matcher, canonicalFieldName, nil
}

// readLinesFromFile is a helper to read a file into a slice of strings, ignoring comments and empty lines.
func ReadLinesFromFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		rawLine := scanner.Text()

		// Check if the line is empty or a comment after trimming for check.
		trimmedForCheck := strings.TrimSpace(rawLine)
		if trimmedForCheck == "" || strings.HasPrefix(trimmedForCheck, "#") {
			continue
		}

		// Add the raw line to be processed by the caller.
		lines = append(lines, rawLine)
	}
	return lines, scanner.Err()
}

// compileStringMatcher handles string values, which can be exact, regex, glob, or status code patterns.
func compileStringMatcher(ctx MatcherContext, value string) (fieldMatcher, error) {
	if strings.HasPrefix(value, "exact:") {
		literalValue := strings.TrimPrefix(value, "exact:")
		return func(entry *LogEntry) bool {
			fieldVal := GetMatchValueIfType(ctx.CanonicalFieldName, entry, StringField)
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
			absoluteFilePath = filepath.Join(filepath.Dir(ctx.FilePath), relativeFilePath)
		}

		// Check if the file is already loaded or needs to be loaded/reloaded
		fileDep, exists := ctx.FileDependencies[absoluteFilePath]
		if !exists || fileDep.CurrentStatus == nil {
			fileDep = &types.FileDependency{Path: absoluteFilePath}
			ctx.FileDependencies[absoluteFilePath] = fileDep
			fileDep.UpdateStatus() // Perform initial status check
		}

		// During the initial load or a reload, we need to read the file content.
		// The watcher is responsible for detecting subsequent changes.
		// Ensure the file's status is up-to-date before processing.
		fileDep.UpdateStatus()
		switch fileDep.CurrentStatus.Status {
		case types.FileStatusLoaded:
			// If the file is loaded, we must read its content for the matcher.
			// The checksum is used by the watcher, but here we need the actual lines.
			lines, readErr := ReadLinesFromFile(absoluteFilePath)
			if readErr != nil {
				// This can happen if the file is deleted between the stat and the read.
				fileDep.CurrentStatus.Status = types.FileStatusError
				fileDep.CurrentStatus.Error = readErr
				logging.LogOutput(logging.LevelWarning, "CONFIG_WARN", "In file '%s': chain '%s', step %d, field '%s': failed to read file '%s' during config load: %v", ctx.FilePath, ctx.ChainName, ctx.StepIndex+1, ctx.CanonicalFieldName, absoluteFilePath, readErr)
				fileDep.Content = []string{}
			} else {
				// Successfully loaded, cache the content for the matcher.
				fileDep.Content = lines
				logging.LogOutput(logging.LevelInfo, "FILE_DEP", "Successfully loaded content from file dependency '%s' for chain '%s', step %d, field '%s'", absoluteFilePath, ctx.ChainName, ctx.StepIndex+1, ctx.CanonicalFieldName)
			}
		case types.FileStatusMissing, types.FileStatusError:
			// If the file is missing or in error, log a warning and treat it as empty.
			logging.LogOutput(logging.LevelWarning, "CONFIG_WARN", "In file '%s': chain '%s', step %d, field '%s': file matcher '%s' is %s, treating as empty. Error: %v", ctx.FilePath, ctx.ChainName, ctx.StepIndex+1, ctx.CanonicalFieldName, absoluteFilePath, fileDep.CurrentStatus.Status, fileDep.CurrentStatus.Error)
			fileDep.Content = []string{}
		}

		// Create a new context for the lines within the file.
		fileCtx := ctx
		fileCtx.FilePath = absoluteFilePath

		// Convert []string to []interface{} to reuse compileListMatcher
		interfaceSlice := make([]interface{}, len(fileDep.Content))
		for i, v := range fileDep.Content {
			interfaceSlice[i] = v
		}
		return compileListMatcher(fileCtx, interfaceSlice)
	} else if strings.HasPrefix(value, "regex:") {
		pattern := strings.TrimPrefix(value, "regex:")
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("in file '%s': chain '%s', step %d, field '%s': invalid regex '%s': %w", ctx.FilePath, ctx.ChainName, ctx.StepIndex+1, ctx.CanonicalFieldName, pattern, err)
		}
		return func(entry *LogEntry) bool {
			fieldVal := GetMatchValueIfType(ctx.CanonicalFieldName, entry, StringField)
			if fieldVal == nil {
				return false
			}
			return re.MatchString(fieldVal.(string))
		}, nil
	} else if strings.HasPrefix(value, "cidr:") {
		if ctx.CanonicalFieldName != "IP" {
			return nil, fmt.Errorf("in file '%s': chain '%s', step %d, field '%s': 'cidr:' matcher is only supported for the 'IP' field", ctx.FilePath, ctx.ChainName, ctx.StepIndex+1, ctx.CanonicalFieldName)
		}
		cidrStr := strings.TrimPrefix(value, "cidr:")
		_, ipNet, err := net.ParseCIDR(cidrStr)
		if err != nil {
			return nil, fmt.Errorf("in file '%s': chain '%s', step %d, field '%s': invalid CIDR '%s' for 'cidr:' matcher: %w", ctx.FilePath, ctx.ChainName, ctx.StepIndex+1, ctx.CanonicalFieldName, cidrStr, err)
		}
		return func(entry *LogEntry) bool {
			fieldVal := GetMatchValueIfType(ctx.CanonicalFieldName, entry, StringField)
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
	if ctx.CanonicalFieldName == "StatusCode" && strings.Contains(strings.ToUpper(value), "X") {
		xIndex := strings.Index(strings.ToUpper(value), "X")
		if xIndex > 0 {
			prefix := value[:xIndex]
			// Ensure the prefix is numeric before creating the matcher.
			if _, err := strconv.Atoi(prefix); err == nil {
				return func(entry *LogEntry) bool {
					fieldVal := GetMatchValueIfType(ctx.CanonicalFieldName, entry, IntField)
					if fieldVal == nil {
						return false
					}
					// Convert the integer to a string for the prefix check.
					return strings.HasPrefix(strconv.Itoa(fieldVal.(int)), prefix)
				}, nil
			}
		}
	}

	// Default case
	trimmedValue := strings.TrimSpace(value)
	// If the trimmed value looks like a directive, it means the raw value had spaces.
	// In that case, we should use the raw value for exact match.
	if strings.HasPrefix(trimmedValue, "exact:") ||
		strings.HasPrefix(trimmedValue, "regex:") ||
		strings.HasPrefix(trimmedValue, "cidr:") ||
		strings.HasPrefix(trimmedValue, "file:") {
		// This is a "spaced-out" directive, treat as literal on the raw value.
		return func(entry *LogEntry) bool {
			fieldVal := GetMatchValueIfType(ctx.CanonicalFieldName, entry, StringField)
			if fieldVal == nil {
				return false
			}
			return fieldVal.(string) == value
		}, nil
	}

	// Otherwise, it's a plain value, use the trimmed value for exact match.
	return func(entry *LogEntry) bool {
		fieldVal := GetMatchValueIfType(ctx.CanonicalFieldName, entry, StringField)
		if fieldVal == nil {
			return false
		}
		return fieldVal.(string) == trimmedValue
	}, nil
}

// compileIntMatcher handles exact integer matches.
func compileIntMatcher(ctx MatcherContext, value int) fieldMatcher {
	return func(entry *LogEntry) bool {
		fieldVal := GetMatchValueIfType(ctx.CanonicalFieldName, entry, IntField)
		if fieldVal == nil {
			return false
		}
		return fieldVal.(int) == value
	}
}

// compileListMatcher handles lists, creating an OR condition over its items.
func compileListMatcher(ctx MatcherContext, values []interface{}) (fieldMatcher, error) {
	var subMatchers []fieldMatcher
	for _, item := range values {
		// Pass the existing context to the sub-matcher.
		// The canonicalFieldName is already set in ctx by compileSingleMatcher.
		matcher, _, err := compileSingleMatcher(ctx, ctx.CanonicalFieldName, item)
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
func compileObjectMatcher(ctx MatcherContext, obj map[string]interface{}) (fieldMatcher, error) {
	var subMatchers []fieldMatcher

	for key, val := range obj {
		var matcher fieldMatcher
		var err error

		switch key {
		case "gt", "gte", "lt", "lte":
			matcher, err = compileRangeMatcher(ctx, key, val)
		case "not":
			matcher, err = compileNotMatcher(ctx, val)
		default:
			return nil, fmt.Errorf("in file '%s': chain '%s', step %d, field '%s': unknown operator '%s' in object matcher", ctx.FilePath, ctx.ChainName, ctx.StepIndex+1, ctx.CanonicalFieldName, key)
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
func compileRangeMatcher(ctx MatcherContext, op string, value interface{}) (fieldMatcher, error) {
	num, ok := value.(int)
	if !ok {
		return nil, fmt.Errorf("in file '%s': chain '%s', step %d, field '%s': value for '%s' must be an integer, got %T", ctx.FilePath, ctx.ChainName, ctx.StepIndex+1, ctx.CanonicalFieldName, op, value)
	}

	// Validate that the field is a known numeric field at compile time.
	// We can do this by calling GetMatchValue with a nil entry and checking the returned type.
	_, fieldType, _ := GetMatchValue(ctx.CanonicalFieldName, nil)
	if fieldType != IntField {
		// This error message is now more generic, which is good.
		return nil, fmt.Errorf("in file '%s': chain '%s', step %d: operator '%s' is only supported for numeric fields, not '%s'", ctx.FilePath, ctx.ChainName, ctx.StepIndex+1, op, ctx.CanonicalFieldName)
	}

	// Return a function that performs the comparison on the generic numeric field.
	return func(entry *LogEntry) bool {
		fieldVal := GetMatchValueIfType(ctx.CanonicalFieldName, entry, IntField)
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
func compileNotMatcher(ctx MatcherContext, value interface{}) (fieldMatcher, error) {
	// The value of 'not' can be a single item or a list of items.
	// We can reuse the existing list and single matcher compilers.
	var subMatcher fieldMatcher
	var err error

	if values, ok := value.([]interface{}); ok {
		// If it's a list, compile it as an OR-matcher.
		subMatcher, err = compileListMatcher(ctx, values)
	} else {
		// Otherwise, compile it as a single matcher.
		subMatcher, _, err = compileSingleMatcher(ctx, ctx.CanonicalFieldName, value)
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
				keyNode.Value = lowerKey
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

// LoadConfigOptions holds the parameters for loading a configuration.
type LoadConfigOptions struct {
	ConfigPath   string
	ExistingDeps map[string]*types.FileDependency
}

func buildDependencyGraph(configPath string) (*depGraph, error) {
	depGraph := newDepGraph()
	scannedFiles := make(map[string]bool)

	var buildGraphRecursive func(currentFile string, isMainConfig bool) error
	buildGraphRecursive = func(currentFile string, isMainConfig bool) error {
		if scannedFiles[currentFile] {
			return nil
		}

		if _, err := os.Stat(currentFile); os.IsNotExist(err) {
			if currentFile == configPath {
				return fmt.Errorf("failed to stat config file %s: %w", currentFile, err)
			}
			return nil
		}

		scannedFiles[currentFile] = true
		depGraph.addNode(currentFile)

		dependencies, _ := findFileDirectives(currentFile, isMainConfig)

		for _, dep := range dependencies {
			depGraph.addEdge(currentFile, dep)
			if err := buildGraphRecursive(dep, false); err != nil {
				return err
			}
		}
		return nil
	}

	if err := buildGraphRecursive(configPath, true); err != nil {
		return nil, err
	}
	return depGraph, nil
}

func parseAndNormalizeYAML(configPath string) (*ChainConfig, []byte, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read YAML file %s: %w", configPath, err)
	}

	logging.LogOutput(logging.LevelDebug, "YAML_DEBUG", "Attempting to unmarshal YAML from %s:\n%s", configPath, string(data))

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		fmt.Fprintf(os.Stderr, "[YAML_ERROR] YAML unmarshalling failed in %s: %v\n", configPath, err)
		return nil, nil, fmt.Errorf("failed to unmarshal YAML: %w", err)
	}

	normalizeYAMLKeys(&root)

	var normalizedYAML bytes.Buffer
	encoder := yaml.NewEncoder(&normalizedYAML)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		return nil, nil, fmt.Errorf("failed to re-marshal normalized YAML: %w", err)
	}

	var config ChainConfig
	decoder := yaml.NewDecoder(&normalizedYAML)
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		var e *yaml.TypeError
		if errors.As(err, &e) {
			fmt.Fprintf(os.Stderr, "[YAML_ERROR] YAML unmarshalling failed in %s: %v\n", configPath, err)
			var errs []string
			for _, msg := range e.Errors {
				errs = append(errs, strings.TrimPrefix(msg, "yaml: "))
			}
			return nil, nil, fmt.Errorf("YAML syntax error in %s: %s", configPath, strings.Join(errs, "; "))
		}
		return nil, nil, fmt.Errorf("failed to strictly unmarshal YAML from %s (unknown field found): %w", configPath, err)
	}
	return &config, data, nil
}

func parseDurations(config *ChainConfig) (time.Duration, time.Duration, time.Duration, time.Duration, time.Duration, error) {
	pollingIntervalStr := DefaultPollingInterval
	if config.Application.Config.PollingInterval != "" {
		pollingIntervalStr = config.Application.Config.PollingInterval
	}
	pollingInterval, err := time.ParseDuration(pollingIntervalStr)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("invalid polling_interval format: %w", err)
	}

	cleanupIntervalStr := DefaultCleanupInterval
	if config.Checker.ActorCleanupInterval != "" {
		cleanupIntervalStr = config.Checker.ActorCleanupInterval
	}
	cleanupInterval, err := time.ParseDuration(cleanupIntervalStr)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("invalid cleanup_interval format: %w", err)
	}

	idleTimeoutStr := DefaultIdleTimeout
	if config.Checker.ActorStateIdleTimeout != "" {
		idleTimeoutStr = config.Checker.ActorStateIdleTimeout
	}
	idleTimeout, err := time.ParseDuration(idleTimeoutStr)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("invalid idle_timeout format: %w", err)
	}

	outOfOrderToleranceStr := DefaultOutOfOrderTolerance
	if config.Parser.OutOfOrderTolerance != "" {
		outOfOrderToleranceStr = config.Parser.OutOfOrderTolerance
	}
	outOfOrderTolerance, err := time.ParseDuration(outOfOrderToleranceStr)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("invalid out_of_order_tolerance format: %w", err)
	}

	eofPollingDelayStr := DefaultEOFPollingDelay
	if config.Application.EOFPollingDelay != "" {
		eofPollingDelayStr = config.Application.EOFPollingDelay
	}
	eofPollingDelay, err := time.ParseDuration(eofPollingDelayStr)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("invalid eof_polling_delay format: %w", err)
	}

	return pollingInterval, cleanupInterval, idleTimeout, outOfOrderTolerance, eofPollingDelay, nil
}

func parseStringAndBoolSettings(config *ChainConfig) (string, string, bool, string, error) {
	logLevelStr := DefaultLogLevel
	if config.Application.LogLevel != "" {
		logLevelStr = config.Application.LogLevel
	}

	lineEndingStr := DefaultLineEnding
	if config.Parser.LineEnding != "" {
		lineEndingStr = config.Parser.LineEnding
	}
	switch lineEndingStr {
	case "lf", "crlf", "cr":
	default:
		return "", "", false, "", fmt.Errorf("invalid line_ending value: '%s'. Must be one of 'lf', 'crlf', 'cr'", lineEndingStr)
	}

	enableMetrics := DefaultEnableMetrics
	if config.Application.EnableMetrics != nil {
		enableMetrics = *config.Application.EnableMetrics
	}

	unblockCooldownStr := DefaultUnblockCooldown
	if config.Checker.UnblockCooldown != "" {
		unblockCooldownStr = config.Checker.UnblockCooldown
	}

	return logLevelStr, lineEndingStr, enableMetrics, unblockCooldownStr, nil
}

func parseCustomLogRegex(config *ChainConfig) (*regexp.Regexp, error) {
	if config.Parser.LogFormatRegex == "" {
		return nil, nil
	}

	re, err := regexp.Compile(config.Parser.LogFormatRegex)
	if err != nil {
		return nil, fmt.Errorf("invalid log_format_regex: %w", err)
	}

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

	return re, nil
}

func parseChains(config *ChainConfig, fileDeps map[string]*types.FileDependency, configPath string, durationTables map[time.Duration]string) ([]BehavioralChain, error) {
	var newChains []BehavioralChain
	var defaultBlockDuration time.Duration
	if config.Blockers.DefaultDuration != "" {
		var err error
		defaultBlockDuration, err = utils.ParseDuration(config.Blockers.DefaultDuration)
		if err != nil {
			return nil, fmt.Errorf("invalid block_duration format for default_block_duration: %w", err)
		}
	}

	for _, yamlChain := range config.Chains {
		if strings.HasPrefix(yamlChain.Action, "!") {
			logging.LogOutput(logging.LevelDebug, "CONFIG_SKIP", "Skipping disabled chain '%s' (action: %s)", yamlChain.Name, yamlChain.Action)
			continue
		}

		var blockDuration time.Duration
		usesDefault := false
		blockDurationStr := yamlChain.BlockDuration
		if yamlChain.BlockDuration != "" {
			var err error
			blockDuration, err = utils.ParseDuration(yamlChain.BlockDuration)
			if err != nil {
				return nil, fmt.Errorf("chain '%s': invalid block_duration format: %w", yamlChain.Name, err)
			}
		} else {
			blockDuration = defaultBlockDuration
			blockDurationStr = config.Blockers.DefaultDuration
			usesDefault = true
		}

		if yamlChain.Action == "block" && blockDuration == 0 {
			logging.LogOutput(logging.LevelWarning, "CONFIG_WARN", "chain '%s' has action 'block' but block_duration is missing or zero and no default is set. This chain will be skipped.", yamlChain.Name)
			continue
		}

		if yamlChain.Action == "block" && blockDuration > 0 {
			if len(durationTables) == 0 {
				logging.LogOutput(logging.LevelWarning, "CONFIG_WARN", "chain '%s' has a block_duration of '%s', but no 'duration_tables' are configured. Block actions for this chain may fail if not in dry-run mode.", yamlChain.Name, blockDurationStr)
			} else if _, ok := durationTables[blockDuration]; !ok {
				logging.LogOutput(logging.LevelWarning, "CONFIG_WARN", "chain '%s' has a block_duration of '%s' which is not defined in 'duration_tables'. Block actions for this chain may fail if not in dry-run mode.", yamlChain.Name, blockDurationStr)
			}
		}

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
			StepsYAML:                yamlChain.Steps,
			MetricsCounter:           new(atomic.Int64),
			MetricsResetCounter:      new(atomic.Int64),
			MetricsHitsCounter:       new(atomic.Int64),
		}

		for i, yamlStep := range yamlChain.Steps {
			numRepeats := yamlStep.Repeated
			if numRepeats < 1 {
				numRepeats = 1
			}

			for r := 0; r < numRepeats; r++ {
				runtimeStep := StepDef{
					Order: len(runtimeChain.Steps) + 1,
				}

				var err error
				if yamlStep.MaxDelay != "" {
					runtimeStep.MaxDelayDuration, err = time.ParseDuration(yamlStep.MaxDelay)
					if err != nil {
						return nil, fmt.Errorf("chain '%s', step %d (repeated %d): invalid max_delay: %w", yamlChain.Name, i+1, r+1, err)
					}
				}
				if yamlStep.MinDelay != "" {
					runtimeStep.MinDelayDuration, err = time.ParseDuration(yamlStep.MinDelay)
					if err != nil {
						return nil, fmt.Errorf("chain '%s', step %d (repeated %d): invalid min_delay: %w", yamlChain.Name, i+1, r+1, err)
					}
				}
				if len(runtimeChain.Steps) == 0 && yamlStep.MinTimeSinceLastHit != "" {
					runtimeStep.MinTimeSinceLastHit, err = time.ParseDuration(yamlStep.MinTimeSinceLastHit)
					if err != nil {
						return nil, fmt.Errorf("chain '%s', step %d: invalid min_time_since_last_hit: %w", yamlChain.Name, i+1, err)
					}
				}

				runtimeStep.Matchers, err = compileMatchers(yamlChain.Name, i, yamlStep.FieldMatches, fileDeps, configPath)
				if err != nil {
					return nil, err
				}
				runtimeChain.Steps = append(runtimeChain.Steps, runtimeStep)
			}
		}
		newChains = append(newChains, runtimeChain)
	}
	return newChains, nil
}

func parseGoodActors(config *ChainConfig, fileDeps map[string]*types.FileDependency, configPath string) ([]GoodActorDef, error) {
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

		for key, value := range goodActorMap {
			baseCtx := MatcherContext{
				ChainName:        fmt.Sprintf("good_actor '%s'", name),
				StepIndex:        0,
				FileDependencies: fileDeps,
				FilePath:         configPath,
			}
			switch strings.ToLower(key) {
			case "ip":
				var ipList []interface{}
				if list, isList := value.([]interface{}); isList {
					ipList = list
				} else {
					ipList = []interface{}{value}
				}
				ipCtx := baseCtx
				ipCtx.CanonicalFieldName = "IP"
				matcher, err := compileListMatcher(ipCtx, ipList)
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
				uaCtx := baseCtx
				uaCtx.CanonicalFieldName = "UserAgent"
				matcher, err := compileListMatcher(uaCtx, uaList)
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
	return newGoodActors, nil
}

func parseDurationTables(config *ChainConfig) (map[time.Duration]string, string, error) {
	newDurationTables := make(map[time.Duration]string, len(config.Blockers.Backends.HAProxy.DurationTables))
	longestDuration := 0 * time.Second
	newFallbackName := ""

	for durationStr, tableName := range config.Blockers.Backends.HAProxy.DurationTables {
		duration, err := utils.ParseDuration(durationStr)
		if err != nil {
			return nil, "", fmt.Errorf("invalid duration '%s' in 'duration_tables': %w", durationStr, err)
		}
		newDurationTables[duration] = tableName

		if duration > longestDuration {
			longestDuration = duration
			newFallbackName = tableName
		}
	}
	return newDurationTables, newFallbackName, nil
}

func parseBlockerSettings(config *ChainConfig) (int, time.Duration, time.Duration, int, int, error) {
	var blockerMaxRetries int
	var blockerRetryDelay, blockerDialTimeout time.Duration
	var blockerCommandQueueSize, blockerCommandsPerSecond int
	var err error

	if config.Blockers.MaxRetries > 0 {
		blockerMaxRetries = config.Blockers.MaxRetries
	} else {
		blockerMaxRetries = DefaultBlockerMaxRetries
	}

	if config.Blockers.RetryDelay != "" {
		blockerRetryDelay, err = time.ParseDuration(config.Blockers.RetryDelay)
		if err != nil {
			return 0, 0, 0, 0, 0, fmt.Errorf("invalid blocker_retry_delay: %w", err)
		}
	} else {
		blockerRetryDelay = DefaultBlockerRetryDelay
	}

	if config.Blockers.DialTimeout != "" {
		blockerDialTimeout, err = time.ParseDuration(config.Blockers.DialTimeout)
		if err != nil {
			return 0, 0, 0, 0, 0, fmt.Errorf("invalid blocker_dial_timeout: %w", err)
		}
	} else {
		blockerDialTimeout = DefaultBlockerDialTimeout
	}

	if config.Blockers.CommandQueueSize > 0 {
		blockerCommandQueueSize = config.Blockers.CommandQueueSize
	} else {
		blockerCommandQueueSize = DefaultBlockerCommandQueueSize
	}

	if config.Blockers.CommandsPerSecond > 0 {
		blockerCommandsPerSecond = config.Blockers.CommandsPerSecond
	} else {
		blockerCommandsPerSecond = DefaultBlockerCommandsPerSecond
	}

	return blockerMaxRetries, blockerRetryDelay, blockerDialTimeout, blockerCommandQueueSize, blockerCommandsPerSecond, nil
}

// LoadConfigFromYAML reads, parses, and pre-compiles regexes for the chains.
func LoadConfigFromYAML(opts LoadConfigOptions) (*LoadedConfig, error) {
	depGraph, err := buildDependencyGraph(opts.ConfigPath)
	if err != nil {
		return nil, err
	}

	if err := depGraph.detectCycle(opts.ConfigPath); err != nil {
		return nil, err
	}

	config, data, err := parseAndNormalizeYAML(opts.ConfigPath)
	if err != nil {
		return nil, err
	}

	if config.Version == "" {
		return nil, fmt.Errorf("configuration file is missing the required 'version' field")
	}

	isSupported := false
	for _, v := range SupportedConfigVersions {
		if config.Version == v {
			isSupported = true
			break
		}
	}
	if !isSupported {
		supportedList := strings.Join(SupportedConfigVersions, ", ")
		return nil, fmt.Errorf("configuration version mismatch: got '%s', this application supports: %s", config.Version, supportedList)
	}

	pollingInterval, cleanupInterval, idleTimeout, outOfOrderTolerance, eofPollingDelay, err := parseDurations(config)
	if err != nil {
		return nil, err
	}

	logLevelStr, lineEndingStr, enableMetrics, unblockCooldownStr, err := parseStringAndBoolSettings(config)
	if err != nil {
		return nil, err
	}

	unblockCooldown, err := time.ParseDuration(unblockCooldownStr)
	if err != nil {
		return nil, fmt.Errorf("invalid unblock_cooldown format: %w", err)
	}

	timestampFormat := AccessLogTimeFormat
	if config.Parser.TimestampFormat != "" {
		timestampFormat = config.Parser.TimestampFormat
	}

	customLogRegex, err := parseCustomLogRegex(config)
	if err != nil {
		return nil, err
	}

	newDurationTables, newFallbackName, err := parseDurationTables(config)
	if err != nil {
		return nil, err
	}

	blockerMaxRetries, blockerRetryDelay, blockerDialTimeout, blockerCommandQueueSize, blockerCommandsPerSecond, err := parseBlockerSettings(config)
	if err != nil {
		return nil, err
	}

	newFileDependencies := make(map[string]*types.FileDependency)
	if opts.ExistingDeps != nil {
		for path, dep := range opts.ExistingDeps {
			newFileDependencies[path] = &types.FileDependency{
				Path:           path,
				PreviousStatus: dep.PreviousStatus.Clone(),
				CurrentStatus:  dep.CurrentStatus.Clone(),
				Content:        dep.Content,
			}
		}
	}

	newChains, err := parseChains(config, newFileDependencies, opts.ConfigPath, newDurationTables)
	if err != nil {
		return nil, err
	}

	newGoodActors, err := parseGoodActors(config, newFileDependencies, opts.ConfigPath)
	if err != nil {
		return nil, err
	}

	var persistenceConfig persistence.PersistenceConfig
	if config.Application.Persistence.Enabled {
		persistenceConfig = persistence.PersistenceConfig{
			Enabled:            true,
			CompactionInterval: config.Application.Persistence.CompactionInterval,
		}
		if persistenceConfig.CompactionInterval == 0 {
			persistenceConfig.CompactionInterval = time.Hour
		}
	}

	var maxTimeSinceLastHit time.Duration
	for _, chain := range newChains {
		if len(chain.Steps) > 0 && chain.Steps[0].MinTimeSinceLastHit > maxTimeSinceLastHit {
			maxTimeSinceLastHit = chain.Steps[0].MinTimeSinceLastHit
		}
	}

	var defaultBlockDuration time.Duration
	if config.Blockers.DefaultDuration != "" {
		defaultBlockDuration, _ = utils.ParseDuration(config.Blockers.DefaultDuration)
	}

	return &LoadedConfig{
		Application: ApplicationConfig{
			LogLevel:        logLevelStr,
			EnableMetrics:   enableMetrics,
			Config:          ConfigManagement{PollingInterval: pollingInterval},
			Persistence:     persistenceConfig,
			EOFPollingDelay: eofPollingDelay,
		},
		Parser: ParserConfig{
			LineEnding:          lineEndingStr,
			OutOfOrderTolerance: outOfOrderTolerance,
			TimestampFormat:     timestampFormat,
		},
		Checker: CheckerConfig{
			UnblockOnGoodActor:    config.Checker.UnblockOnGoodActor,
			UnblockCooldown:       unblockCooldown,
			ActorCleanupInterval:  cleanupInterval,
			ActorStateIdleTimeout: idleTimeout,
			MaxTimeSinceLastHit:   maxTimeSinceLastHit,
		},
		Blockers: BlockersConfig{
			DefaultDuration:   defaultBlockDuration,
			CommandsPerSecond: blockerCommandsPerSecond,
			CommandQueueSize:  blockerCommandQueueSize,
			DialTimeout:       blockerDialTimeout,
			MaxRetries:        blockerMaxRetries,
			RetryDelay:        blockerRetryDelay,
			Backends: Backends{
				HAProxy: HAProxyConfig{
					Addresses:         config.Blockers.Backends.HAProxy.Addresses,
					DurationTables:    newDurationTables,
					TableNameFallback: newFallbackName,
				},
			},
		},
		GoodActors:       newGoodActors,
		Chains:           newChains,
		FileDependencies: newFileDependencies,
		LogFormatRegex:   customLogRegex,
		YAMLContent:      data,
	}, nil
}

// logFileDependencyChanges logs changes in file dependencies between old and new configurations.
func logFileDependencyChanges(p *Processor, oldDeps, newDeps map[string]*types.FileDependency) {
	var added, removed, modified []string

	// Check for added or modified files
	for path, newDep := range newDeps {
		oldDep, exists := oldDeps[path]
		if !exists {
			added = append(added, fmt.Sprintf("'%s' (Status: %s)", path, newDep.CurrentStatus.Status))
		} else {
			// Compare CurrentStatus of oldDep with CurrentStatus of newDep
			// This is the crucial part: oldDep.CurrentStatus represents the state *before* the reload.
			// newDep.CurrentStatus represents the state *after* the reload.
			if oldDep.CurrentStatus == nil || newDep.CurrentStatus == nil {
				// This case should ideally not happen if both maps are populated correctly,
				// but as a safeguard, treat as modified if status structs are missing.
				modified = append(modified, fmt.Sprintf("'%s' (status struct missing in comparison)", path))
			} else if oldDep.CurrentStatus.Status != newDep.CurrentStatus.Status {
				modified = append(modified, fmt.Sprintf("'%s' (status changed from %s to %s)", path, oldDep.CurrentStatus.Status, newDep.CurrentStatus.Status))
			} else if oldDep.CurrentStatus.Checksum != newDep.CurrentStatus.Checksum {
				modified = append(modified, fmt.Sprintf("'%s' (content changed - checksum mismatch)", path))
			}
		}
	}

	// Check for removed files
	for path := range oldDeps {
		if _, exists := newDeps[path]; !exists {
			removed = append(removed, fmt.Sprintf("'%s'", path))
		}
	}

	if len(added) > 0 {
		p.LogFunc(logging.LevelInfo, "FILE_DEP", "Added file dependencies: %s", strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		p.LogFunc(logging.LevelInfo, "FILE_DEP", "Removed file dependencies: %s", strings.Join(removed, ", "))
	}
	if len(modified) > 0 {
		p.LogFunc(logging.LevelInfo, "FILE_DEP", "Modified file dependencies: %s", strings.Join(modified, ", "))
	}
}

// reloadConfiguration contains the logic to reload configuration, compare for changes,
// and update the processor state. It is designed to be called by either the
// file watcher or the signal reloader.
// findNewlyAddedGoodActors compares old and new good actor lists and returns newly added ones.
func findNewlyAddedGoodActors(oldActors, newActors []GoodActorDef) []GoodActorDef {
	oldSet := make(map[string]bool)
	for _, actor := range oldActors {
		oldSet[actor.Name] = true
	}

	var newlyAdded []GoodActorDef
	for _, actor := range newActors {
		if !oldSet[actor.Name] {
			newlyAdded = append(newlyAdded, actor)
		}
	}
	return newlyAdded
}

// unblockNewlyWhitelistedIPs checks all currently blocked IPs against newly added good actors
// and unblocks those that match. Uses a hybrid approach: fast path for exact IPs (O(1)),
// slow path for CIDR/regex patterns (O(N)).
func unblockNewlyWhitelistedIPs(p *Processor, newGoodActors []GoodActorDef) {
	if len(newGoodActors) == 0 {
		return
	}

	p.persistenceMutex.Lock()
	blockedCount := len(p.activeBlocks)
	p.persistenceMutex.Unlock()

	if blockedCount == 0 {
		return
	}

	unblocked := 0
	slowPathCount := 0

	for _, goodActor := range newGoodActors {
		// Check if this good actor has IP matchers
		if len(goodActor.IPMatchers) == 0 {
			continue // No IP matcher, skip
		}

		// We need to iterate through activeBlocks for pattern matching (CIDR/regex)
		// Create a temporary log entry for testing
		p.persistenceMutex.Lock()
		for ip := range p.activeBlocks {
			// Create a minimal LogEntry with just the IP set
			testEntry := &LogEntry{
				IPInfo: utils.NewIPInfo(ip),
			}

			// Test if this IP matches the good actor's IP matcher
			if goodActor.IPMatchers[0](testEntry) {
				// Match found - need to unblock
				blockInfo := p.activeBlocks[ip]
				delete(p.activeBlocks, ip)

				// Issue unblock command to HAProxy
				if !p.DryRun && p.Blocker != nil {
					ipInfo := utils.NewIPInfo(ip)
					_ = p.Blocker.Unblock(ipInfo, "added-to-good-actors")
				}

				p.LogFunc(logging.LevelInfo, "UNBLOCK_WHITELIST",
					"Unblocked %s (was blocked by %s): newly added to good_actors (%s)",
					ip, blockInfo.Reason, goodActor.Name)
				unblocked++
			}
		}
		p.persistenceMutex.Unlock()
		slowPathCount++
	}

	if unblocked > 0 {
		p.LogFunc(logging.LevelInfo, "UNBLOCK_WHITELIST",
			"Checked %d blocked IPs against %d new good actor rule(s), unblocked %d IPs",
			blockedCount, slowPathCount, unblocked)
	}
}

func reloadConfiguration(p *Processor, mainConfigChanged bool, oldConfigForComparison *AppConfig) { //nolint:cyclop
	p.ConfigMutex.RLock()
	oldChains := p.Chains
	// Use the provided oldConfigForComparison instead of cloning here.
	oldConfig := oldConfigForComparison
	oldLogRegex := p.LogRegex
	p.ConfigMutex.RUnlock()

	var newLastModTime time.Time
	if mainConfigChanged {
		fileInfo, err := os.Stat(p.ConfigPath)
		if err != nil {
			p.LogFunc(logging.LevelError, "WATCH_ERROR", "Failed to stat file for ModTime update %s: %v", p.ConfigPath, err)
			newLastModTime = oldConfig.LastModTime // Fallback to old time on error
		} else {
			newLastModTime = fileInfo.ModTime()
		}
	} else {
		newLastModTime = oldConfig.LastModTime // Preserve old time if main config didn't change
	}

	opts := LoadConfigOptions{
		ConfigPath:   p.ConfigPath,
		ExistingDeps: oldConfig.FileDependencies,
	}
	loadedCfg, err := LoadConfigFromYAML(opts)
	if err != nil {
		p.LogFunc(logging.LevelError, "LOAD_ERROR", "Failed to reload configuration: %v", err)
		return // The deferred signal will still fire.
	}

	// Create a new AppConfig from the loaded configuration.
	newAppConfig := &AppConfig{
		Application:      loadedCfg.Application,
		Parser:           loadedCfg.Parser,
		Checker:          loadedCfg.Checker,
		Blockers:         loadedCfg.Blockers,
		GoodActors:       loadedCfg.GoodActors,
		FileDependencies: loadedCfg.FileDependencies,

		// Preserve mockable functions and set the correct LastModTime.
		StatFunc:    oldConfig.StatFunc,
		LastModTime: newLastModTime,

		YAMLContent: loadedCfg.YAMLContent,
	}

	// Update the processor's state with the new config.
	p.ConfigMutex.Lock()
	p.Chains = loadedCfg.Chains
	p.Config = newAppConfig // Atomically swap the config pointer.
	// The LastModTime is already set correctly in newAppConfig, no need to update here.
	p.LogRegex = loadedCfg.LogFormatRegex
	p.EnableMetrics = loadedCfg.Application.EnableMetrics // Set the processor's EnableMetrics field
	initializeMetrics(p, loadedCfg)

	logging.SetLogLevel(loadedCfg.Application.LogLevel)
	p.ConfigMutex.Unlock()

	// --- Compare and log general config changes ---
	configChanged := compareConfigs(*oldConfig, *loadedCfg) ||
		(oldLogRegex != nil) != (loadedCfg.LogFormatRegex != nil)

	if configChanged {
		logConfigurationSummary(p)
	}

	// --- Compare and log file dependency changes ---
	logFileDependencyChanges(p, oldConfig.FileDependencies, loadedCfg.FileDependencies)

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

	// --- Unblock IPs that match newly added good actors ---
	if newAppConfig.Checker.UnblockOnGoodActor {
		newlyAdded := findNewlyAddedGoodActors(oldConfig.GoodActors, loadedCfg.GoodActors)
		if len(newlyAdded) > 0 {
			unblockNewlyWhitelistedIPs(p, newlyAdded)
		}
	}

}

// initializeMetrics sets up all the metric counters based on the loaded configuration.
// It resets and repopulates the metric maps, making it safe to call on both startup and reload.
func initializeMetrics(p *Processor, loadedCfg *LoadedConfig) {
	if !p.EnableMetrics {
		// If metrics are disabled, ensure all metric maps are nil or empty
		p.Metrics.ChainsCompleted = nil
		p.Metrics.ChainsReset = nil
		p.Metrics.ChainsHits = nil
		p.Metrics.MatchKeyHits = nil
		p.Metrics.BlockDurations = nil
		p.Metrics.CmdsPerBlocker = nil
		p.Metrics.GoodActorHits = nil
		return
	}

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
	for duration := range loadedCfg.Blockers.Backends.HAProxy.DurationTables {
		p.Metrics.BlockDurations.Store(duration, new(atomic.Int64))
	}
	if loadedCfg.Blockers.DefaultDuration > 0 {
		p.Metrics.BlockDurations.Store(loadedCfg.Blockers.DefaultDuration, new(atomic.Int64))
	}

	// Initialize per-blocker command counters.
	p.Metrics.CmdsPerBlocker = &sync.Map{}
	for _, addr := range loadedCfg.Blockers.Backends.HAProxy.Addresses {
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
	var signalName string
	// If ReloadOn is not specified, default to SIGHUP.
	if p.ReloadOn == "" {
		signalName = "HUP"
	} else {
		signalName = strings.ToUpper(p.ReloadOn)
	}

	// The main function should have already validated the signal name.
	// This check is now just a safeguard, especially for dry-run mode.
	if _, ok := signalMap[signalName]; !ok || p.DryRun {
		p.LogFunc(logging.LevelDebug, "SIGNAL", "Signal-based config reloading is disabled or signal is unsupported.")
		return
	}

	// The signal channel is already notified by the caller in main.go.

	p.LogFunc(logging.LevelInfo, "SIGNAL", "Signal-based config reloading enabled. Send %s signal to reload.", signalName)

	for {
		select {
		case <-stop:
			p.LogFunc(logging.LevelInfo, "SIGNAL", "SignalReloader received stop signal. Shutting down.")
			return
		case s := <-signalCh:
			p.LogFunc(logging.LevelInfo, "SIGNAL", "Received signal %s. Reloading configuration...", s)
			func() { // Use an anonymous function to scope the defer correctly.
				// Defer the test signal to ensure it's sent whether the reload succeeds or fails.
				if p.TestSignals != nil && p.TestSignals.ReloadDoneSignal != nil {
					defer func() { p.TestSignals.ReloadDoneSignal <- struct{}{} }()
				}
				// When reloading via signal, we don't have an "old" config from the watcher's perspective.
				// We need to clone the current config to serve as the oldConfigForComparison.
				p.ConfigMutex.RLock()
				currentConfig := p.Config.Clone()
				p.ConfigMutex.RUnlock()
				reloadConfiguration(p, true, &currentConfig)
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
	pollingInterval := p.Config.Application.Config.PollingInterval
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

		// Clone the current config *before* checking for changes in file dependencies.
		// This ensures that oldConfigForComparison accurately represents the state before any updates.
		p.ConfigMutex.RLock()
		oldConfigForComparison := p.Config.Clone()
		p.ConfigMutex.RUnlock()

		isChanged := false
		changedFile := ""
		mainFileChanged := false

		// 1. Check the main YAML file
		fileInfo, err := os.Stat(p.ConfigPath)
		if err != nil {
			p.LogFunc(logging.LevelError, "WATCH_ERROR", "Failed to stat file %s: %v", p.ConfigPath, err)
			continue
		}

		p.ConfigMutex.RLock()
		if fileInfo.ModTime().After(p.Config.LastModTime) {
			isChanged = true
			mainFileChanged = true
			changedFile = p.ConfigPath
		} else {
			// 2. Check all file dependencies if YAML hasn't changed
			for path, fileDep := range p.Config.FileDependencies {
				fileDep.UpdateStatus()
				if fileDep.HasChanged() {
					isChanged = true
					changedFile = path
					break
				}
			}
		}
		p.ConfigMutex.RUnlock()

		if isChanged {
			p.LogFunc(logging.LevelInfo, "WATCH", "Detected change in '%s'. Attempting reload...", changedFile)
			func() { // Use an anonymous function to scope the defer correctly.
				defer func() {
					if r := recover(); r != nil {
						p.LogFunc(logging.LevelError, "WATCH_PANIC", "Recovered from panic during config reload: %v", r)
					}
				}()
				// Defer the test signal to ensure it's sent whether the reload succeeds or fails.
				if p.TestSignals != nil && p.TestSignals.ReloadDoneSignal != nil {
					defer func() { p.TestSignals.ReloadDoneSignal <- struct{}{} }()
				}
				reloadConfiguration(p, mainFileChanged, &oldConfigForComparison)
			}()
		}
	}
}
