package config

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
	"sync/atomic"
	"syscall"
	"time"

	"bot-detector/internal/cluster"
	"bot-detector/internal/logging"
	"bot-detector/internal/types"
	"bot-detector/internal/utils"

	"gopkg.in/yaml.v3"
)

// AreChainsSemanticallyEqual compares two BehavioralChain structs for logical equality,
// ignoring non-comparable fields like function pointers (Matchers).
// Exported for use by app package.
func AreChainsSemanticallyEqual(a, b BehavioralChain) bool {
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

// CompareConfigs checks if two configurations are semantically different.
// Exported for use by app package.
func CompareConfigs(oldCfg AppConfig, newCfg LoadedConfig) bool {
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
func (g *depGraph) detectCycle(configFilePath string) error {
	visiting := make(map[string]bool)
	visited := make(map[string]bool)
	// Start the DFS from the first node added to the graph, which is the main config file.
	// This makes cycle detection deterministic, which is important for testing.
	// We find the root node by looking for a node that is not a dependency of any other node.
	// A simpler and sufficient approach for this codebase is to start from the known root config path.
	if path, err := g.dfs(g.Nodes[configFilePath], visiting, visited, nil); err != nil {
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

// FieldMatcher is a function type that represents a compiled matching rule.
// It takes a LogEntry and returns true if the entry satisfies the rule.
type FieldMatcher func(entry *types.LogEntry) bool

// CompileMatchers parses the raw `field_matches` interface from YAML into a slice of efficient matcher functions.
// Exported for use in tests.
func CompileMatchers(chainName string, stepIndex int, fieldMatches map[string]interface{}, fileDeps map[string]*types.FileDependency, filePath string) ([]struct {
	Matcher   FieldMatcher
	FieldName string
}, error) {
	var matchers []struct {
		Matcher   FieldMatcher
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
			Matcher   FieldMatcher
			FieldName string
		}{Matcher: matcher, FieldName: fieldName})
	}
	return matchers, nil
}

// compileSingleMatcher is a large switch that handles the different value "shapes" (string, int, list, map).
func compileSingleMatcher(ctx MatcherContext, field string, value interface{}) (FieldMatcher, string, error) {
	// Convert the incoming fieldName to its canonical PascalCase form for internal matching.
	// This ensures that YAML keys like "ip" map correctly to LogEntry.IPInfo.
	canonicalFieldName, ok := types.FieldNameCanonicalMap[strings.ToLower(field)]
	if !ok {
		// If not found in the map, assume the fieldName is already canonical or unknown.
		canonicalFieldName = field
	}

	// Create a new context with the canonical field name for subsequent matchers.
	subCtx := ctx
	subCtx.CanonicalFieldName = canonicalFieldName

	var matcher FieldMatcher
	var err error

	switch v := value.(type) {
	case string:
		matcher, err = CompileStringMatcher(subCtx, v)
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

// compileStringMatcher handles string values, which can be exact, regex, glob, or status code patterns.
func CompileStringMatcher(ctx MatcherContext, value string) (FieldMatcher, error) {
	if strings.HasPrefix(value, "exact:") {
		literalValue := strings.TrimPrefix(value, "exact:")
		return func(entry *types.LogEntry) bool {
			fieldVal := types.GetMatchValueIfType(ctx.CanonicalFieldName, entry, types.StringField)
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
			lines, readErr := utils.ReadLinesFromFile(absoluteFilePath)
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
		// Generate cache key for matcher result caching
		cacheKey := ctx.CanonicalFieldName + ":regex:" + pattern
		return func(entry *types.LogEntry) bool {
			// Check matcher result cache first
			if result, found := entry.CheckMatcherCache(cacheKey); found {
				return result
			}

			// Cache miss - evaluate matcher
			fieldVal := types.GetMatchValueIfType(ctx.CanonicalFieldName, entry, types.StringField)
			if fieldVal == nil {
				entry.StoreMatcherResult(cacheKey, false)
				return false
			}
			result := re.MatchString(fieldVal.(string))

			// Store result in cache
			entry.StoreMatcherResult(cacheKey, result)
			return result
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
		// Generate cache key for matcher result caching
		cacheKey := ctx.CanonicalFieldName + ":cidr:" + cidrStr
		return func(entry *types.LogEntry) bool {
			// Check matcher result cache first
			if result, found := entry.CheckMatcherCache(cacheKey); found {
				return result
			}

			// Cache miss - evaluate matcher
			fieldVal := types.GetMatchValueIfType(ctx.CanonicalFieldName, entry, types.StringField)
			if fieldVal == nil {
				entry.StoreMatcherResult(cacheKey, false)
				return false
			}
			ipStr := fieldVal.(string)
			ip := net.ParseIP(ipStr)
			if ip == nil {
				entry.StoreMatcherResult(cacheKey, false)
				return false
			}
			result := ipNet.Contains(ip)

			// Store result in cache
			entry.StoreMatcherResult(cacheKey, result)
			return result
		}, nil
	}

	// Special handling for status code patterns like "4XX"
	if ctx.CanonicalFieldName == "StatusCode" && strings.Contains(strings.ToUpper(value), "X") {
		xIndex := strings.Index(strings.ToUpper(value), "X")
		if xIndex > 0 {
			prefix := value[:xIndex]
			// Ensure the prefix is numeric before creating the matcher.
			if _, err := strconv.Atoi(prefix); err == nil {
				return func(entry *types.LogEntry) bool {
					fieldVal := types.GetMatchValueIfType(ctx.CanonicalFieldName, entry, types.IntField)
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
		return func(entry *types.LogEntry) bool {
			fieldVal := types.GetMatchValueIfType(ctx.CanonicalFieldName, entry, types.StringField)
			if fieldVal == nil {
				return false
			}
			return fieldVal.(string) == value
		}, nil
	}

	// Otherwise, it's a plain value, use the trimmed value for exact match.
	return func(entry *types.LogEntry) bool {
		fieldVal := types.GetMatchValueIfType(ctx.CanonicalFieldName, entry, types.StringField)
		if fieldVal == nil {
			return false
		}
		return fieldVal.(string) == trimmedValue
	}, nil
}

// compileIntMatcher handles exact integer matches.
func compileIntMatcher(ctx MatcherContext, value int) FieldMatcher {
	return func(entry *types.LogEntry) bool {
		fieldVal := types.GetMatchValueIfType(ctx.CanonicalFieldName, entry, types.IntField)
		if fieldVal == nil {
			return false
		}
		return fieldVal.(int) == value
	}
}

// compileListMatcher handles lists, creating an OR condition over its items.
func compileListMatcher(ctx MatcherContext, values []interface{}) (FieldMatcher, error) {
	var subMatchers []FieldMatcher
	for _, item := range values {
		// Pass the existing context to the sub-matcher.
		// The canonicalFieldName is already set in ctx by compileSingleMatcher.
		matcher, _, err := compileSingleMatcher(ctx, ctx.CanonicalFieldName, item)
		if err != nil {
			return nil, err // Error in a sub-matcher
		}
		subMatchers = append(subMatchers, matcher)
	}

	return func(entry *types.LogEntry) bool {
		for _, matcher := range subMatchers {
			if matcher(entry) {
				return true // OR logic: one match is enough
			}
		}
		return false
	}, nil
}

// compileObjectMatcher handles map values, creating an AND condition for its sub-matchers.
func compileObjectMatcher(ctx MatcherContext, obj map[string]interface{}) (FieldMatcher, error) {
	var subMatchers []FieldMatcher

	for key, val := range obj {
		var matcher FieldMatcher
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

	return func(entry *types.LogEntry) bool {
		for _, matcher := range subMatchers {
			if !matcher(entry) {
				return false // AND logic: one failure means total failure
			}
		}
		return true
	}, nil
}

// compileRangeMatcher handles numeric range operators (gt, gte, lt, lte).
func compileRangeMatcher(ctx MatcherContext, op string, value interface{}) (FieldMatcher, error) {
	num, ok := value.(int)
	if !ok {
		return nil, fmt.Errorf("in file '%s': chain '%s', step %d, field '%s': value for '%s' must be an integer, got %T", ctx.FilePath, ctx.ChainName, ctx.StepIndex+1, ctx.CanonicalFieldName, op, value)
	}

	// Validate that the field is a known numeric field at compile time.
	// We can do this by calling GetMatchValue with a nil entry and checking the returned type.
	_, fieldType, _ := types.GetMatchValue(ctx.CanonicalFieldName, nil)
	if fieldType != types.IntField {
		// This error message is now more generic, which is good.
		return nil, fmt.Errorf("in file '%s': chain '%s', step %d: operator '%s' is only supported for numeric fields, not '%s'", ctx.FilePath, ctx.ChainName, ctx.StepIndex+1, op, ctx.CanonicalFieldName)
	}

	// Return a function that performs the comparison on the generic numeric field.
	return func(entry *types.LogEntry) bool {
		fieldVal := types.GetMatchValueIfType(ctx.CanonicalFieldName, entry, types.IntField)
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
func compileNotMatcher(ctx MatcherContext, value interface{}) (FieldMatcher, error) {
	// The value of 'not' can be a single item or a list of items.
	// We can reuse the existing list and single matcher compilers.
	var subMatcher FieldMatcher
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
	return func(entry *types.LogEntry) bool {
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
				// Normalize to lowercase for case-insensitive configuration.
				keyNode.Value = strings.ToLower(keyNode.Value)
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
	ConfigFilePath string
	ExistingDeps   map[string]*types.FileDependency
}

func buildDependencyGraph(configFilePath string) (*depGraph, error) {
	depGraph := newDepGraph()
	scannedFiles := make(map[string]bool)

	var buildGraphRecursive func(currentFile string, isMainConfig bool) error
	buildGraphRecursive = func(currentFile string, isMainConfig bool) error {
		if scannedFiles[currentFile] {
			return nil
		}

		if _, err := os.Stat(currentFile); os.IsNotExist(err) {
			if currentFile == configFilePath {
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

	if err := buildGraphRecursive(configFilePath, true); err != nil {
		return nil, err
	}
	return depGraph, nil
}

func parseAndNormalizeYAML(configFilePath string) (*TopLevelConfig, []byte, error) {
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read YAML file %s: %w", configFilePath, err)
	}

	logging.LogOutput(logging.LevelDebug, "YAML_DEBUG", "Attempting to unmarshal YAML from %s:\n%s", configFilePath, string(data))

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		fmt.Fprintf(os.Stderr, "[YAML_ERROR] YAML unmarshalling failed in %s: %v\n", configFilePath, err)
		return nil, nil, fmt.Errorf("failed to unmarshal YAML: %w", err)
	}

	normalizeYAMLKeys(&root)

	var normalizedYAML bytes.Buffer
	encoder := yaml.NewEncoder(&normalizedYAML)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		return nil, nil, fmt.Errorf("failed to re-marshal normalized YAML: %w", err)
	}

	var config TopLevelConfig
	decoder := yaml.NewDecoder(&normalizedYAML)
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		var e *yaml.TypeError
		if errors.As(err, &e) {
			fmt.Fprintf(os.Stderr, "[YAML_ERROR] YAML unmarshalling failed in %s: %v\n", configFilePath, err)
			var errs []string
			for _, msg := range e.Errors {
				errs = append(errs, strings.TrimPrefix(msg, "yaml: "))
			}
			return nil, nil, fmt.Errorf("YAML syntax error in %s: %s", configFilePath, strings.Join(errs, "; "))
		}
		return nil, nil, fmt.Errorf("failed to strictly unmarshal YAML from %s (unknown field found): %w", configFilePath, err)
	}
	return &config, data, nil
}

func parseDurations(config *TopLevelConfig) (time.Duration, time.Duration, time.Duration, time.Duration, time.Duration, error) {
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

// parseClusterConfig parses the cluster configuration from YAML and converts it to cluster.ClusterConfig.
// Returns nil if no cluster configuration is provided (single-node mode).
func parseClusterConfig(config *TopLevelConfig) (*cluster.ClusterConfig, error) {
	// Cluster configuration is optional
	if config.Cluster == nil {
		return nil, nil
	}

	// Parse config_poll_interval
	configPollIntervalStr := DefaultConfigPollInterval
	if config.Cluster.ConfigPollInterval != "" {
		configPollIntervalStr = config.Cluster.ConfigPollInterval
	}
	configPollInterval, err := time.ParseDuration(configPollIntervalStr)
	if err != nil {
		return nil, fmt.Errorf("invalid cluster.config_poll_interval format: %w", err)
	}

	// Parse metrics_report_interval
	metricsReportIntervalStr := DefaultMetricsReportInterval
	if config.Cluster.MetricsReportInterval != "" {
		metricsReportIntervalStr = config.Cluster.MetricsReportInterval
	}
	metricsReportInterval, err := time.ParseDuration(metricsReportIntervalStr)
	if err != nil {
		return nil, fmt.Errorf("invalid cluster.metrics_report_interval format: %w", err)
	}

	// Parse protocol
	protocol := DefaultClusterProtocol
	if config.Cluster.Protocol != "" {
		protocol = config.Cluster.Protocol
	}

	// Convert nodes from YAML format to cluster format
	var nodes []cluster.NodeConfig
	for _, node := range config.Cluster.Nodes {
		nodes = append(nodes, cluster.NodeConfig{
			Name:    node.Name,
			Address: node.Address,
		})
	}

	clusterConfig := &cluster.ClusterConfig{
		Nodes:                 nodes,
		ConfigPollInterval:    configPollInterval,
		MetricsReportInterval: metricsReportInterval,
		Protocol:              protocol,
	}

	// Validate the cluster configuration
	if err := clusterConfig.Validate(); err != nil {
		return nil, fmt.Errorf("invalid cluster configuration: %w", err)
	}

	return clusterConfig, nil
}

func parseStringAndBoolSettings(config *TopLevelConfig) (string, string, bool, string, error) {
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

func parseCustomLogRegex(config *TopLevelConfig) (*regexp.Regexp, error) {
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

func parseChains(config *TopLevelConfig, fileDeps map[string]*types.FileDependency, configFilePath string, durationTables map[time.Duration]string) ([]BehavioralChain, error) {
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

				runtimeStep.Matchers, err = CompileMatchers(yamlChain.Name, i, yamlStep.FieldMatches, fileDeps, configFilePath)
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

func parseGoodActors(config *TopLevelConfig, fileDeps map[string]*types.FileDependency, configFilePath string) ([]GoodActorDef, error) {
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
				FilePath:         configFilePath,
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
				def.IPMatchers = []FieldMatcher{matcher}
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
				def.UAMatchers = []FieldMatcher{matcher}
			}
		}

		if len(def.IPMatchers) > 0 || len(def.UAMatchers) > 0 {
			newGoodActors = append(newGoodActors, def)
		}
	}
	return newGoodActors, nil
}

func parseDurationTables(config *TopLevelConfig) (map[time.Duration]string, string, error) {
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

func parseBlockerSettings(config *TopLevelConfig) (int, time.Duration, time.Duration, int, int, int, error) {
	var blockerMaxRetries int
	var blockerRetryDelay, blockerDialTimeout time.Duration
	var blockerCommandQueueSize, blockerCommandsPerSecond, maxCommandsPerBatch int
	var err error

	if config.Blockers.MaxRetries > 0 {
		blockerMaxRetries = config.Blockers.MaxRetries
	} else {
		blockerMaxRetries = DefaultBlockerMaxRetries
	}

	if config.Blockers.RetryDelay != "" {
		blockerRetryDelay, err = time.ParseDuration(config.Blockers.RetryDelay)
		if err != nil {
			return 0, 0, 0, 0, 0, 0, fmt.Errorf("invalid blocker_retry_delay: %w", err)
		}
	} else {
		blockerRetryDelay = DefaultBlockerRetryDelay
	}

	if config.Blockers.DialTimeout != "" {
		blockerDialTimeout, err = time.ParseDuration(config.Blockers.DialTimeout)
		if err != nil {
			return 0, 0, 0, 0, 0, 0, fmt.Errorf("invalid blocker_dial_timeout: %w", err)
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

	if config.Blockers.MaxCommandsPerBatch > 0 {
		maxCommandsPerBatch = config.Blockers.MaxCommandsPerBatch
	} else {
		maxCommandsPerBatch = DefaultMaxCommandsPerBatch
	}

	return blockerMaxRetries, blockerRetryDelay, blockerDialTimeout, blockerCommandQueueSize, blockerCommandsPerSecond, maxCommandsPerBatch, nil
}

// LoadConfigFromYAML reads, parses, and pre-compiles regexes for the chains.
func LoadConfigFromYAML(opts LoadConfigOptions) (*LoadedConfig, error) {
	depGraph, err := buildDependencyGraph(opts.ConfigFilePath)
	if err != nil {
		return nil, err
	}

	if err := depGraph.detectCycle(opts.ConfigFilePath); err != nil {
		return nil, err
	}

	config, data, err := parseAndNormalizeYAML(opts.ConfigFilePath)
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

	clusterConfig, err := parseClusterConfig(config)
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

	blockerMaxRetries, blockerRetryDelay, blockerDialTimeout, blockerCommandQueueSize, blockerCommandsPerSecond, maxCommandsPerBatch, err := parseBlockerSettings(config)
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

	newChains, err := parseChains(config, newFileDependencies, opts.ConfigFilePath, newDurationTables)
	if err != nil {
		return nil, err
	}

	newGoodActors, err := parseGoodActors(config, newFileDependencies, opts.ConfigFilePath)
	if err != nil {
		return nil, err
	}

	persistenceConfig := config.Application.Persistence
	if persistenceConfig.CompactionInterval == 0 {
		persistenceConfig.CompactionInterval = time.Hour
	}

	var maxTimeSinceLastHit time.Duration
	for _, chain := range newChains {
		if len(chain.Steps) > 0 && chain.Steps[0].MinTimeSinceLastHit > maxTimeSinceLastHit {
			maxTimeSinceLastHit = chain.Steps[0].MinTimeSinceLastHit
		}
	}

	// Validate unique names
	if err := validateUniqueNames(newChains, newGoodActors); err != nil {
		return nil, err
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
			DefaultDuration:     defaultBlockDuration,
			CommandsPerSecond:   blockerCommandsPerSecond,
			CommandQueueSize:    blockerCommandQueueSize,
			MaxCommandsPerBatch: maxCommandsPerBatch,
			DialTimeout:         blockerDialTimeout,
			MaxRetries:          blockerMaxRetries,
			RetryDelay:          blockerRetryDelay,
			Backends: Backends{
				HAProxy: HAProxyConfig{
					Addresses:         config.Blockers.Backends.HAProxy.Addresses,
					DurationTables:    newDurationTables,
					TableNameFallback: newFallbackName,
				},
			},
		},
		Cluster:          clusterConfig,
		GoodActors:       newGoodActors,
		Chains:           newChains,
		FileDependencies: newFileDependencies,
		LogFormatRegex:   customLogRegex,
		YAMLContent:      data,
	}, nil
}

// validateUniqueNames checks that chain names and good actor names are unique
func validateUniqueNames(chains []BehavioralChain, goodActors []GoodActorDef) error {
	// Check chain names
	chainNames := make(map[string]bool, len(chains))
	for _, chain := range chains {
		if chainNames[chain.Name] {
			return fmt.Errorf("duplicate chain name: '%s'", chain.Name)
		}
		chainNames[chain.Name] = true
	}

	// Check good actor names
	goodActorNames := make(map[string]bool, len(goodActors))
	for _, actor := range goodActors {
		if goodActorNames[actor.Name] {
			return fmt.Errorf("duplicate good_actors name: '%s'", actor.Name)
		}
		goodActorNames[actor.Name] = true
	}

	return nil
}
