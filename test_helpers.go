package main

import (
	"bot-detector/internal/blocker"
	"bot-detector/internal/logging"
	"bot-detector/internal/metrics"
	"bot-detector/internal/store"
	"bot-detector/internal/utils"
	"flag"
	"fmt"
	"github.com/stretchr/testify/mock"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// IsTesting returns true if the code is running as part of a "go test" command.
// It works by checking for the presence of the "-test.v" or "-test.run" arguments,
// which are automatically added by the Go testing framework. This is more robust
// than `flag.Lookup` when the global flag set is manipulated during tests.
func IsTesting() bool {
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "-test.") {
			return true
		}
	}
	return false
}

// muteGlobalLogger redirects the output of the standard logger to discard,
// effectively silencing any direct calls to log.Printf during tests.
func muteGlobalLogger() {
	log.SetOutput(io.Discard)
}

// resetGlobalState resets global variables to their default state for test isolation.
// This is critical for tests that modify global state, such as command-line flags.
func resetGlobalState() {
	muteGlobalLogger()

	// Reset the global flag set to clear any flags parsed in other tests. This is still
	// good practice, even if we don't have many global flags anymore.
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
}

// MockBlocker implements the Blocker interface for testing, allowing Block() calls to be intercepted.
type MockBlocker struct {
	mock.Mock
	BlockFunc                  func(ipInfo utils.IPInfo, duration time.Duration, reason string) error
	UnblockFunc                func(ipInfo utils.IPInfo, reason string) error
	ListBlockedFunc            func() ([]string, error)
	CompareHAProxyBackendsFunc func(expTolerance time.Duration) ([]blocker.SyncDiscrepancy, error)
}

// Block calls the stored mock function to simulate the blocking action.
func (m *MockBlocker) Block(ipInfo utils.IPInfo, duration time.Duration, reason string) error {
	if m.BlockFunc != nil {
		return m.BlockFunc(ipInfo, duration, reason)
	}
	args := m.Called(ipInfo, duration, reason)
	return args.Error(0)
}

// Unblock calls the stored mock function to simulate the unblocking action.
func (m *MockBlocker) Unblock(ipInfo utils.IPInfo, reason string) error {
	if m.UnblockFunc != nil {
		return m.UnblockFunc(ipInfo, reason)
	}
	args := m.Called(ipInfo, reason)
	return args.Error(0)
}

func (m *MockBlocker) DumpBackends() ([]string, error) {
	if m.ListBlockedFunc != nil {
		return m.ListBlockedFunc()
	}
	args := m.Called()
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]string), args.Error(1)
}

// CompareHAProxyBackends calls the stored mock function to simulate comparing HAProxy backends.
func (m *MockBlocker) CompareHAProxyBackends(expTolerance time.Duration) ([]blocker.SyncDiscrepancy, error) {
	if m.CompareHAProxyBackendsFunc != nil {
		return m.CompareHAProxyBackendsFunc(expTolerance)
	}
	args := m.Called(expTolerance)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]blocker.SyncDiscrepancy), args.Error(1)
}

// newTestProcessor creates a new Processor instance with sensible defaults for testing.
func newTestProcessor(config *AppConfig, chains []BehavioralChain) *Processor {
	if config == nil {
		config = &AppConfig{}
	}
	p := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		// Blocker will be set below
		ConfigMutex:       &sync.RWMutex{},
		Metrics:           metrics.NewMetrics(),
		Chains:            chains,
		Config:            config,
		LogFunc:           func(level logging.LogLevel, tag string, format string, args ...interface{}) {},
		EntryBuffer:       make([]*LogEntry, 0),
		TopActorsPerChain: make(map[string]map[string]*store.ActorStats),
		EnableMetrics:     config.EnableMetrics,

		NowFunc: time.Now, // Default to real time for tests unless overridden.
	}
	// Ensure StatFunc and FileOpener are never nil to prevent panics.
	if p.Config != nil {
		if p.Config.StatFunc == nil {
			p.Config.StatFunc = defaultStatFunc
		}
		if p.Config.FileOpener == nil {
			p.Config.FileOpener = func(name string) (fileHandle, error) {
				return os.Open(name)
			}
		}
	}
	// Use a no-op mock blocker by default for most tests.
	p.Blocker = &MockBlocker{}
	// Initialize signalFlush to prevent nil pointer dereference in tests.
	p.oooBufferFlushSignal = make(chan struct{}, 1)
	p.signalOooBufferFlush = p.doSignalOooBufferFlush
	p.CheckChainsFunc = func(entry *LogEntry) { CheckChains(p, entry) }
	return p
}

// dryRunTestHarness encapsulates the common setup for DryRunLogProcessor tests.
type dryRunTestHarness struct {
	t              *testing.T
	processor      *Processor
	tempLogFile    string
	capturedLogs   []string
	processedLines []string
	logMutex       sync.Mutex
}

// newDryRunTestHarness creates and initializes a test harness for DryRunLogProcessor.
func newDryRunTestHarness(t *testing.T, config *AppConfig) *dryRunTestHarness {
	t.Helper()

	h := &dryRunTestHarness{t: t}

	if config == nil {
		config = &AppConfig{}
	}

	// Create temp file and set global path
	tempDir := t.TempDir()
	h.tempLogFile = filepath.Join(tempDir, "test_dryrun.log")

	// Create processor with mock/capture functions
	h.processor = newTestProcessor(config, nil)
	h.processor.NowFunc = func() time.Time { return time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC) } // Mock time.Now()

	// Use a custom LogFunc to capture logs and identify skipped lines.
	// This needs to be done before setting ProcessLogLine, as ProcessLogLine
	// will call processLogLineInternal, which in turn calls LogFunc.
	h.processor.LogPath = h.tempLogFile
	h.processor.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) { //nolint:gocritic
		h.logMutex.Lock()
		defer h.logMutex.Unlock()
		logLine := fmt.Sprintf(tag+": "+format, args...)
		h.capturedLogs = append(h.capturedLogs, logLine)
	}

	// Override ProcessLogLine to use the real processing logic and capture processed lines.
	h.processor.ProcessLogLine = func(line string) {
		// Call the actual log line processing function.
		processLogLineInternal(h.processor, line)

		// Check if the line was *not* skipped by processLogLineInternal.
		// We do this by checking if a "Skipped (Comment/Empty)" log was *not* generated
		// for this specific line. This is a bit indirect but avoids modifying
		// processLogLineInternal's return signature. This logic is now simpler since
		// we only care if the line content itself is empty or a comment.
		h.logMutex.Lock()
		defer h.logMutex.Unlock()
		trimmedLine := strings.TrimSpace(line)
		skippedLogFound := trimmedLine == "" || strings.HasPrefix(trimmedLine, "#")

		if !skippedLogFound {
			// If no skipped log was found for this line, it means it was processed.
			h.processedLines = append(h.processedLines, line)
		}
	}
	return h
}

// checkerTestHarness encapsulates common setup for CheckChains tests.
type checkerTestHarness struct {
	t             *testing.T
	processor     *Processor
	blockCalled   bool
	unblockCalled bool
	blockCallArgs struct {
		ipInfo   utils.IPInfo
		duration time.Duration
	}
	capturedLogs []string
	logMutex     sync.Mutex
}

// newCheckerTestHarness creates a harness for testing CheckChains logic.
func newCheckerTestHarness(t *testing.T, config *AppConfig) *checkerTestHarness {
	t.Helper()
	resetGlobalState()

	h := &checkerTestHarness{t: t}

	// Setup a mock blocker to intercept calls.
	mockBlocker := &MockBlocker{
		BlockFunc: func(ipInfo utils.IPInfo, duration time.Duration, reason string) error {
			h.blockCalled = true
			h.blockCallArgs.ipInfo = ipInfo
			h.blockCallArgs.duration = duration
			return nil
		},
		UnblockFunc: func(ipInfo utils.IPInfo, reason string) error {
			h.unblockCalled = true
			return nil
		},
	}

	// Create the processor with mock functions.
	h.processor = newTestProcessor(config, nil) // Start with no chains.
	h.processor.Blocker = mockBlocker
	h.processor.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		h.logMutex.Lock()
		defer h.logMutex.Unlock()
		h.capturedLogs = append(h.capturedLogs, fmt.Sprintf(tag+": "+format, args...))
	}

	return h
}

// addChain compiles a chain from its YAML definition and adds it to the processor.
func (h *checkerTestHarness) addChain(chainYAML BehavioralChain) {
	h.t.Helper()
	// This simulates the compilation part of LoadConfigFromYAML for a single chain.
	runtimeChain := chainYAML
	// Create an empty FileDependencies map for testing purposes.
	testFileDependencies := make(map[string]*FileDependency)

	for i, stepYAML := range chainYAML.StepsYAML {
		matchers, err := compileMatchers(chainYAML.Name, i, stepYAML.FieldMatches, testFileDependencies, "")
		if err != nil {
			h.t.Fatalf("Failed to compile matchers for chain '%s': %v", chainYAML.Name, err)
		}
		runtimeChain.Steps = append(runtimeChain.Steps, StepDef{
			Order:    i + 1,
			Matchers: matchers,
		})
	}
	h.processor.Chains = append(h.processor.Chains, runtimeChain)
}

// processEntry runs a single log entry through the CheckChains logic.
func (h *checkerTestHarness) processEntry(entry *LogEntry) {
	h.t.Helper()
	CheckChains(h.processor, entry)
}

// assertChainProgress checks if a given key is at the expected step for a chain.
func (h *checkerTestHarness) assertChainProgress(chainName string, entry *LogEntry, expectedStep int) {
	h.t.Helper()
	key := GetActor(&h.processor.Chains[0], entry)
	h.processor.ActivityMutex.RLock()
	defer h.processor.ActivityMutex.RUnlock()
	activity, exists := h.processor.ActivityStore[store.Actor(key)]
	if !exists || activity.ChainProgress[chainName].CurrentStep != expectedStep {
		h.t.Errorf("Expected chain '%s' to be at step %d, but it was not. Activity: %+v", chainName, expectedStep, activity)
	}
}

// assertBlocked checks if a given key is marked as blocked.
func (h *checkerTestHarness) assertBlocked(entry *LogEntry, expected bool) { //nolint:thelper
	h.t.Helper()
	key := GetActor(&h.processor.Chains[0], entry)
	h.processor.ActivityMutex.RLock()
	defer h.processor.ActivityMutex.RUnlock()
	activity, exists := h.processor.ActivityStore[store.Actor(key)]
	if !exists && expected {
		h.t.Errorf("Expected activity for key %+v to exist and be blocked, but it doesn't exist.", key)
		return
	}
	if exists && activity.IsBlocked != expected {
		h.t.Errorf("Expected IsBlocked to be %t, but it was %t for key %+v", expected, activity.IsBlocked, key)
	}
}

// assertChainProgressCleared checks that a chain's progress has been removed from the activity store.
func (h *checkerTestHarness) assertChainProgressCleared(chainName string, entry *LogEntry) {
	h.t.Helper()
	key := GetActor(&h.processor.Chains[0], entry)
	h.processor.ActivityMutex.RLock()
	defer h.processor.ActivityMutex.RUnlock()
	activity, exists := h.processor.ActivityStore[store.Actor(key)]
	if exists && len(activity.ChainProgress) != 0 {
		h.t.Errorf("Expected ChainProgress to be cleared for key %+v, but it has %d entries: %v", key, len(activity.ChainProgress), activity.ChainProgress)
	}
}
