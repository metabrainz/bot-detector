package main

import (
	"bot-detector/internal/logging"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall" // Added syscall import
	"testing"
	"time"
)

// concurrentBuffer provides a thread-safe buffer.
type concurrentBuffer struct {
	b bytes.Buffer
	m sync.Mutex
}

// Write appends data to the buffer in a thread-safe manner.
func (b *concurrentBuffer) Write(p []byte) (int, error) {
	b.m.Lock()
	defer b.m.Unlock()
	return b.b.Write(p)
}

// String returns the content of the buffer as a string.
func (b *concurrentBuffer) String() string {
	b.m.Lock()
	defer b.m.Unlock()
	return b.b.String()
}

// Reset clears the buffer.
func (b *concurrentBuffer) Reset() {
	b.m.Lock()
	defer b.m.Unlock()
	b.b.Reset()
}

// LoadConfigFromYAMLTest is a test helper that loads a YAML configuration file
// by temporarily setting the global YAMLFilePath.
func LoadConfigFromYAMLTest(path string) (*LoadedConfig, error) {
	originalPath := YAMLFilePath
	YAMLFilePath = path
	defer func() { YAMLFilePath = originalPath }()
	return LoadConfigFromYAML()
}

// setupTestProcessor initializes a Processor for testing with a given dryRun mode.
// It captures log output for verification.
func setupTestProcessor(t *testing.T, dryRun bool, logFilePath string) (*Processor, *concurrentBuffer) {
	t.Helper()

	var logOutput concurrentBuffer
	logFunc := func(level logging.LogLevel, logType, format string, args ...interface{}) {
		// We only care about ACTION logs for this test.
		if logType == "ACTION" {
			msg := fmt.Sprintf(format, args...)
			logOutput.Write([]byte(msg + "\n"))
		}
	}

	// Load base configuration from a test file.
	testYAMLPath := "testdata/chains.yaml"
	loadedCfg, err := LoadConfigFromYAMLTest(testYAMLPath)
	if err != nil {
		t.Fatalf("Failed to load test YAML config: %v", err)
	}

	// Create a minimal AppConfig for the test.
	appConfig := &AppConfig{
		CleanupInterval:      1 * time.Minute,
		DefaultBlockDuration: 1 * time.Hour,
		EOFPollingDelay:      10 * time.Millisecond,
		FileDependencies:     []string{testYAMLPath},
		MaxTimeSinceLastHit:  5 * time.Minute,
		OutOfOrderTolerance:  2 * time.Second,
		TimestampFormat:      "02/Jan/2006:15:04:05 -0700",
		StatFunc: func(path string) (os.FileInfo, error) {
			// For the purpose of this test, we only need to return a mock FileInfo
			// that has a non-nil Sys() value. The actual values don't matter
			// as we are not testing file rotation.
			return &mockFileInfo{
				size: 1024,
				sys:  &syscall.Stat_t{}, // Non-nil value to prevent panic
			}, nil
		},
	}

	// Initialize the processor.
	p := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[TrackingKey]*BotActivity),
		ConfigMutex:   &sync.RWMutex{},
		Chains:        loadedCfg.Chains,
		Config:        appConfig,
		LogRegex:      regexp.MustCompile(`(\S+) \S+ \S+ \[([^\]]+)\] "(\S+) (\S+) \S+" (\d+) \d+ "\S+" "([^"]+)"`),
		DryRun:        dryRun,
		LogFunc:       logFunc,
		NowFunc:       time.Now,
		signalCh:      make(chan os.Signal, 1),
	}
	p.IsWhitelistedFunc = func(ipInfo IPInfo) bool { return IsIPWhitelisted(p, ipInfo) }
	p.CheckChainsFunc = func(entry *LogEntry) { CheckChains(p, entry) }
	p.ProcessLogLine = func(line string, lineNumber int) { processLogLineInternal(p, line, lineNumber) }

	// Set the global LogFilePath for the processor to use.
	LogFilePath = logFilePath

	return p, &logOutput
}

// TestDryRunVsLiveModeComparison compares the behavior of dry-run and live tailing modes.
func TestDryRunVsLiveModeComparison(t *testing.T) {
	// 1. Create a temporary log file with test data.
	logFile, err := os.CreateTemp(t.TempDir(), "test_access.log")
	if err != nil {
		t.Fatalf("Failed to create temp log file: %v", err)
	}
	logFilePath, err := filepath.Abs(logFile.Name())
	if err != nil {
		t.Fatalf("Failed to get absolute path for temp log file: %v", err)
	}

	// Test log data with in-order, out-of-order, and a buggy line.
	logData := "\n"
	logData += "1.2.3.4 - - [10/Nov/2025:10:00:00 -0700] \"GET /path1 HTTP/1.1\" 200 123 \"-\" \"TestAgent1\"\n"
	logData += "1.2.3.4 - - [10/Nov/2025:10:00:02 -0700] \"GET /path1 HTTP/1.1\" 200 123 \"-\" \"TestAgent1\"\n"
	logData += "1.2.3.4 - - [10/Nov/2025:10:00:01 -0700] \"GET /path1 HTTP/1.1\" 200 123 \"-\" \"TestAgent1\"\n"
	logData += "5.6.7.8 - - [10/Nov/2025:10:00:03 -0700] \"GET /path2 HTTP/1.1\" 404 456 \"-\" \"TestAgent2\"\n"
	logData += "this-is-a-buggy-line\n"
	logData += "1.2.3.4 - - [10/Nov/2025:10:00:03 -0700] \"GET /path1 HTTP/1.1\" 200 123 \"-\" \"TestAgent1\"\n"

	if _, err := logFile.WriteString(strings.TrimSpace(logData)); err != nil {
		t.Fatalf("Failed to write to temp log file: %v", err)
	}
	logFile.Close()

	// 2. Run in Dry-Run Mode
	dryRunProcessor, dryRunLogs := setupTestProcessor(t, true, logFilePath)
	dryRunDone := make(chan struct{})
	go DryRunLogProcessor(dryRunProcessor, dryRunDone)
	<-dryRunDone

	// 3. Run in Live Mode
	liveProcessor, liveLogs := setupTestProcessor(t, false, logFilePath)
	liveSignalCh := make(chan os.Signal, 1)
	liveReadySignal := make(chan struct{}, 1)

	go func() {
		LiveLogTailer(liveProcessor, liveSignalCh, liveReadySignal)
	}()

	// Wait for the tailer to be ready and process the initial content.
	<-liveReadySignal

	// Give it a moment to process the lines.
	time.Sleep(liveProcessor.Config.EOFPollingDelay * 5)

	// Send shutdown signal.
	liveSignalCh <- os.Interrupt
	// Allow time for shutdown.
	time.Sleep(100 * time.Millisecond)

	// 4. Compare the results
	dryRunOutput := dryRunLogs.String()
	liveOutput := liveLogs.String()

	if dryRunOutput != liveOutput {
		t.Errorf("Dry-run and live mode outputs differ.\n\nDry-run output:\n%s\nLive mode output:\n%s", dryRunOutput, liveOutput)
	}
}
