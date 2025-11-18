package testutil

import (
	"bot-detector/internal/checker"
	"bot-detector/internal/app"
	"bot-detector/internal/blocker"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/logparser"
	"bot-detector/internal/metrics"
	"bot-detector/internal/processor"
	"bot-detector/internal/store"
	"bot-detector/internal/utils"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
)

// IsTesting is re-exported from utils for backward compatibility.
var IsTesting = utils.IsTesting

// muteGlobalLogger redirects the output of the standard logger to discard,
// effectively silencing any direct calls to log.Printf during tests.
func muteGlobalLogger() {
	log.SetOutput(io.Discard)
}

// resetGlobalState resets global variables to their default state for test isolation.
// This is critical for tests that modify global state, such as command-line flags.
func ResetGlobalState() {
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

// Shutdown is a no-op for the mock to satisfy the Blocker interface.
func (m *MockBlocker) Shutdown() {
	// No-op for mock
}

// newTestProcessor creates a new Processor instance with sensible defaults for testing.
func NewTestProcessor(cfg *config.AppConfig, chains []config.BehavioralChain) *app.Processor {
	if cfg == nil {
		cfg = &config.AppConfig{}
	}
	p := &app.Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		// Blocker will be set below
		ConfigMutex:       &sync.RWMutex{},
		Metrics:           metrics.NewMetrics(),
		Chains:            chains,
		Config:            cfg,
		LogFunc:           func(level logging.LogLevel, tag string, format string, args ...interface{}) {},
		EntryBuffer:       make([]*app.LogEntry, 0),
		TopActorsPerChain: make(map[string]map[string]*store.ActorStats),
		EnableMetrics:     cfg.Application.EnableMetrics,

		NowFunc: time.Now, // Default to real time for tests unless overridden.
	}
	// Ensure StatFunc and FileOpener are never nil to prevent panics.
	if p.Config != nil {
		if p.Config.StatFunc == nil {
			p.Config.StatFunc = processor.DefaultStatFunc
		}
		if p.Config.FileOpener == nil {
			p.Config.FileOpener = func(name string) (config.FileHandle, error) {
				return os.Open(name)
			}
		}
	}
	// Use a no-op mock blocker by default for most tests.
	p.Blocker = &MockBlocker{}
	// Initialize signalFlush to prevent nil pointer dereference in tests.
	p.OooBufferFlushSignal = make(chan struct{}, 1)
	p.SignalOooBufferFlush = func() { checker.DoSignalOooBufferFlush(p) }
	p.CheckChainsFunc = func(entry *app.LogEntry) { checker.CheckChains(p, entry) }
	return p
}

// dryRunTestHarness encapsulates the common setup for DryRunLogProcessor tests.
type dryRunTestHarness struct {
	t              *testing.T
	processor      *app.Processor
	tempLogFile    string
	capturedLogs   []string
	processedLines []string
	logMutex       sync.Mutex
}

// newDryRunTestHarness creates and initializes a test harness for DryRunLogProcessor.
func newDryRunTestHarness(t *testing.T, cfg *config.AppConfig) *dryRunTestHarness {
	t.Helper()

	h := &dryRunTestHarness{t: t}

	if cfg == nil {
		cfg = &config.AppConfig{}
	}

	// Create temp file and set global path
	tempDir := t.TempDir()
	h.tempLogFile = filepath.Join(tempDir, "test_dryrun.log")

	// Create processor with mock/capture functions
	h.processor = NewTestProcessor(cfg, nil)
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
		logparser.ProcessLogLineInternal(h.processor, line)

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

