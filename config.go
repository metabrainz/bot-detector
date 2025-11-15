package main

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
	"bot-detector/internal/utils"
	"bufio"
	"bytes"
	"crypto/sha256"
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

	"gopkg.in/yaml.v3"
)

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

// calculateChecksum computes the SHA256 checksum of a slice of strings.
func calculateChecksum(lines []string) string {
	h := sha256.New()
	h.Write([]byte(strings.Join(lines, "\n")))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// --- New Matcher Compilation Logic ---

// FileDependencyStatus holds the complete state of a file at a specific point in time.
type FileDependencyStatus struct {
	ModTime  time.Time
	Status   FileStatus
	Checksum string // SHA256 checksum of the content to detect no-op changes.
	Error    error  // Store the error if status is Error
}

// FileDependency represents a file that the configuration depends on, tracking its state over time.
type FileDependency struct {
	Path           string
	PreviousStatus *FileDependencyStatus
	CurrentStatus  *FileDependencyStatus
	Content        []string // Cached content from the last successful load
}

// FileStatus indicates the current state of a file dependency.
type FileStatus int

const (
	FileStatusUnknown FileStatus = iota
	FileStatusLoaded
	FileStatusMissing
	FileStatusError
)

func (fs FileStatus) String() string {
	switch fs {
	case FileStatusUnknown:
		return "Unknown"
	case FileStatusLoaded:
		return "Loaded"
	case FileStatusMissing:
		return "Missing"
	case FileStatusError:
		return "Error"
	default:
		return fmt.Sprintf("FileStatus(%d)", fs)
	}
}

// updateStatus polls the file on disk and updates its CurrentStatus.
// It always preserves the previous state before updating.
func (fd *FileDependency) updateStatus() {
	debug := false
	if debug {
		logging.LogOutput(logging.LevelDebug, "FILE_DEP_UPDATE", "Updating status for '%s'. PreviousStatus: %+v", fd.Path, fd.PreviousStatus)
	}

	// Preserve the last known state.
	fd.PreviousStatus = fd.CurrentStatus

	newStatus := &FileDependencyStatus{}

	info, err := os.Stat(fd.Path)
	if err != nil {
		if os.IsNotExist(err) {
			newStatus.Status = FileStatusMissing
			newStatus.Error = err
			if debug {
				logging.LogOutput(logging.LevelDebug, "FILE_DEP_UPDATE", "File '%s' is missing. NewStatus: %+v", fd.Path, newStatus)
			}
		} else {
			// Another error occurred (e.g., permissions).
			newStatus.Status = FileStatusError
			newStatus.Error = err
			if debug {
				logging.LogOutput(logging.LevelError, "FILE_DEP_UPDATE", "Error stating file '%s': %v. NewStatus: %+v", fd.Path, err, newStatus)
			}
		}
		fd.CurrentStatus = newStatus
		return
	}

	// File exists, update basic info.
	newStatus.Status = FileStatusLoaded
	newStatus.ModTime = info.ModTime()

	// Always attempt to read the file if it exists and is loaded, to detect read errors (e.g., permissions).
	// This also ensures the checksum is always up-to-date if content changes without ModTime update (rare, but possible).
	content, readErr := ReadLinesFromFile(fd.Path)
	if readErr != nil {
		newStatus.Status = FileStatusError
		newStatus.Error = readErr
		if debug {
			logging.LogOutput(logging.LevelError, "FILE_DEP_UPDATE", "Error reading file '%s': %v. NewStatus: %+v", fd.Path, readErr, newStatus)
		}
	} else {
		newStatus.Checksum = calculateChecksum(content)
		if debug {
			logging.LogOutput(logging.LevelDebug, "FILE_DEP_UPDATE", "File '%s' read successfully. Checksum: %s", fd.Path, newStatus.Checksum)
		}
	}

	fd.CurrentStatus = newStatus
	if debug {
		logging.LogOutput(logging.LevelDebug, "FILE_DEP_UPDATE", "Finished updating status for '%s'. CurrentStatus: %+v", fd.Path, fd.CurrentStatus)
	}
}

// hasChanged compares the PreviousStatus and CurrentStatus to see if a reload is warranted.
func (fd *FileDependency) hasChanged() bool {
	if fd.PreviousStatus == nil {
		// If there's no previous state, any loaded state is a "change".
		return fd.CurrentStatus.Status == FileStatusLoaded
	}

	// A change is warranted if the status is different, or if the checksums don't match.
	// A simple `touch` will change ModTime but not Checksum, so we rely on checksum.
	return fd.CurrentStatus.Status != fd.PreviousStatus.Status ||
		fd.CurrentStatus.Checksum != fd.PreviousStatus.Checksum
}

// Clone creates a deep copy of the FileDependencyStatus object.
func (fds *FileDependencyStatus) Clone() *FileDependencyStatus {
	if fds == nil {
		return nil
	}
	return &FileDependencyStatus{
		ModTime:  fds.ModTime,
		Status:   fds.Status,
		Checksum: fds.Checksum,
		Error:    fds.Error, // errors are immutable
	}
}

// Clone creates a deep copy of the FileDependency object.
func (fd *FileDependency) Clone() *FileDependency {
	if fd == nil {
		return nil
	}
	// Content is a slice of strings, which is a reference type, but since the
	// content itself is immutable strings, a shallow copy of the slice is sufficient.
	contentCopy := make([]string, len(fd.Content))
	copy(contentCopy, fd.Content)

	return &FileDependency{
		Path:           fd.Path,
		PreviousStatus: fd.PreviousStatus.Clone(),
		CurrentStatus:  fd.CurrentStatus.Clone(),
		Content:        contentCopy,
	}
}

// MatcherContext holds common parameters needed during matcher compilation.
type MatcherContext struct {
	ChainName          string
	StepIndex          int
	CanonicalFieldName string
	FileDependencies   map[string]*FileDependency
	FilePath           string
}

// fieldMatcher is a function type that represents a compiled matching rule.
// It takes a LogEntry and returns true if the entry satisfies the rule.
type fieldMatcher func(entry *LogEntry) bool

// compileMatchers parses the raw `field_matches` interface from YAML into a slice of efficient matcher functions.
func compileMatchers(chainName string, stepIndex int, fieldMatches map[string]interface{}, fileDeps map[string]*FileDependency, filePath string) ([]struct {
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
			fileDep = &FileDependency{Path: absoluteFilePath}
			ctx.FileDependencies[absoluteFilePath] = fileDep
			fileDep.updateStatus() // Perform initial status check
		}

		// During the initial load or a reload, we need to read the file content.
		// The watcher is responsible for detecting subsequent changes.
		// Ensure the file's status is up-to-date before processing.
		fileDep.updateStatus()
		switch fileDep.CurrentStatus.Status {
		case FileStatusLoaded:
			// If the file is loaded, we must read its content for the matcher.
			// The checksum is used by the watcher, but here we need the actual lines.
			lines, readErr := ReadLinesFromFile(absoluteFilePath)
			if readErr != nil {
				// This can happen if the file is deleted between the stat and the read.
				fileDep.CurrentStatus.Status = FileStatusError
				fileDep.CurrentStatus.Error = readErr
				logging.LogOutput(logging.LevelWarning, "CONFIG_WARN", "In file '%s': chain '%s', step %d, field '%s': failed to read file '%s' during config load: %v", ctx.FilePath, ctx.ChainName, ctx.StepIndex+1, ctx.CanonicalFieldName, absoluteFilePath, readErr)
				fileDep.Content = []string{}
			} else {
				// Successfully loaded, cache the content for the matcher.
				fileDep.Content = lines
				logging.LogOutput(logging.LevelInfo, "FILE_DEP", "Successfully loaded content from file dependency '%s' for chain '%s', step %d, field '%s'", absoluteFilePath, ctx.ChainName, ctx.StepIndex+1, ctx.CanonicalFieldName)
			}
		case FileStatusMissing, FileStatusError:
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
	ExistingDeps map[string]*FileDependency
}

// LoadConfigFromYAML reads, parses, and pre-compiles regexes for the chains.
func LoadConfigFromYAML(opts LoadConfigOptions) (*LoadedConfig, error) {
	// --- PHASE 1: Build Dependency Graph and Detect Cycles ---
	depGraph := newDepGraph()
	scannedFiles := make(map[string]bool)

	// Use a recursive function to build the graph. This helps trace the correct path for cycle detection.
	var buildGraphRecursive func(currentFile string, isMainConfig bool) error
	buildGraphRecursive = func(currentFile string, isMainConfig bool) error {
		if scannedFiles[currentFile] {
			return nil
		}

		// The main config file MUST exist. Dependencies can be missing.
		if _, err := os.Stat(currentFile); os.IsNotExist(err) {
			if currentFile == opts.ConfigPath {
				return fmt.Errorf("failed to stat config file %s: %w", currentFile, err)
			}
			// For dependencies, this is not a fatal error for the graph building phase.
			// The compilation phase will handle it as a warning.
			return nil
		}

		scannedFiles[currentFile] = true
		depGraph.addNode(currentFile)

		// Errors from findFileDirectives are now considered non-fatal for this phase, so we ignore the error.
		dependencies, _ := findFileDirectives(currentFile, isMainConfig)

		for _, dep := range dependencies {
			depGraph.addEdge(currentFile, dep)
			if err := buildGraphRecursive(dep, false); err != nil {
				return err
			}
		}
		return nil
	}

	if err := buildGraphRecursive(opts.ConfigPath, true); err != nil {
		return nil, err
	}

	// --- PHASE 2: Detect Cycles ---
	if err := depGraph.detectCycle(opts.ConfigPath); err != nil {
		return nil, err // Return the clear cycle error
	}

	data, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read YAML file %s: %w", opts.ConfigPath, err)
	}

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
		var e *yaml.TypeError
		if errors.As(err, &e) {
			var errs []string
			for _, msg := range e.Errors {
				errs = append(errs, strings.TrimPrefix(msg, "yaml: "))
			}
			return nil, fmt.Errorf("YAML syntax error in %s: %s", opts.ConfigPath, strings.Join(errs, "; "))
		}
		return nil, fmt.Errorf("failed to strictly unmarshal YAML from %s (unknown field found): %w", opts.ConfigPath, err)
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
	enableMetrics := DefaultEnableMetrics

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

	// Handle EnableMetrics with a pointer to bool
	if config.EnableMetrics != nil {
		enableMetrics = *config.EnableMetrics
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
		return nil, fmt.Errorf("invalid polling_interval format: %w", err)
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
		duration, err := utils.ParseDuration(durationStr)
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
	// Use the existing map if provided, otherwise create a new one.
	// This preserves the PreviousStatus across reloads.
	newFileDependencies := make(map[string]*FileDependency)
	if opts.ExistingDeps != nil {
		for path, dep := range opts.ExistingDeps {
			// Create a new FileDependency object for the new config,
			// but preserve the PreviousStatus from the old config.
			newFileDependencies[path] = &FileDependency{
				Path:           path,
				PreviousStatus: dep.PreviousStatus.Clone(), // Deep copy
				CurrentStatus:  dep.CurrentStatus.Clone(),  // Deep copy
				Content:        dep.Content,
			}
		}
	}

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
			numRepeats := yamlStep.Repeated
			if numRepeats < 1 {
				numRepeats = 1 // Default to 1 if not specified or invalid
			}

			for r := 0; r < numRepeats; r++ {
				runtimeStep := StepDef{
					Order: len(runtimeChain.Steps) + 1, // Assign a unique order to each generated step
				}

				// Parse delays (apply to each repeated step)
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
				// min_time_since_last_hit only applies to the first step of a chain.
				// It should only be applied to the very first step generated, not subsequent repeated steps.
				if len(runtimeChain.Steps) == 0 && yamlStep.MinTimeSinceLastHit != "" {
					runtimeStep.MinTimeSinceLastHit, err = time.ParseDuration(yamlStep.MinTimeSinceLastHit)
					if err != nil {
						return nil, fmt.Errorf("chain '%s', step %d: invalid min_time_since_last_hit: %w", yamlChain.Name, i+1, err)
					}
				}

				// Compile the new flexible matchers
				runtimeStep.Matchers, err = compileMatchers(yamlChain.Name, i, yamlStep.FieldMatches, newFileDependencies, opts.ConfigPath)
				if err != nil {
					return nil, err // Error from compilation
				}
				runtimeChain.Steps = append(runtimeChain.Steps, runtimeStep)
			}
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
			// Create a base context for good actors.
			baseCtx := MatcherContext{
				ChainName:        fmt.Sprintf("good_actor '%s'", name),
				StepIndex:        0,                   // Good actors don't have steps, use 0 or a sentinel value
				FileDependencies: newFileDependencies, // Now using the map
				FilePath:         opts.ConfigPath,
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

	// Parse persistence settings
	var persistenceConfig persistence.PersistenceConfig
	if config.Persistence.Enabled {
		persistenceConfig = persistence.PersistenceConfig{
			Enabled:            true,
			StateDir:           config.Persistence.StateDir,
			CompactionInterval: config.Persistence.CompactionInterval,
		}
		if persistenceConfig.CompactionInterval == 0 {
			persistenceConfig.CompactionInterval = time.Hour // Default to 1 hour
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
		FileDependencies:         newFileDependencies,
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
		EnableMetrics:            enableMetrics,
		Persistence:              persistenceConfig,
		YAMLContent:              data,
	}, nil
}

// logFileDependencyChanges logs changes in file dependencies between old and new configurations.
func logFileDependencyChanges(p *Processor, oldDeps, newDeps map[string]*FileDependency) {
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
		MaxTimeSinceLastHit:      loadedCfg.MaxTimeSinceLastHit,
		OutOfOrderTolerance:      loadedCfg.OutOfOrderTolerance,
		PollingInterval:          loadedCfg.PollingInterval,
		TimestampFormat:          loadedCfg.TimestampFormat,
		UnblockOnGoodActor:       loadedCfg.UnblockOnGoodActor,
		UnblockCooldown:          loadedCfg.UnblockCooldown,
		EnableMetrics:            loadedCfg.EnableMetrics,
		Persistence:              loadedCfg.Persistence,

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
	p.EnableMetrics = loadedCfg.EnableMetrics // Set the processor's EnableMetrics field
	initializeMetrics(p, loadedCfg)

	logging.SetLogLevel(loadedCfg.LogLevel)
	p.ConfigMutex.Unlock()

	// --- Compare and log general config changes ---
	configChanged := compareConfigsByTag(*oldConfig, *loadedCfg) ||
		loadedCfg.LogLevel != logging.GetLogLevel().String() ||
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
				fileDep.updateStatus()
				if fileDep.hasChanged() {
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
