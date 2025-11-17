package main

import (
	"bot-detector/internal/blocker"
	"bot-detector/internal/logging"
	metrics "bot-detector/internal/metrics"
	"bot-detector/internal/store"
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
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

// setupTestProcessor initializes a Processor for testing with a given dryRun mode.
// It captures log output for verification.
func setupTestProcessor(t *testing.T, dryRun bool, logFilePath string) (*Processor, *concurrentBuffer, chan struct{}) {
	t.Helper()

	var logOutput concurrentBuffer
	logFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		// For this comparison test, we want to capture all relevant log types
		// and normalize them so the output is identical between modes.
		switch tag { //nolint:gocritic
		case "METRICS":
			// Ignore all metrics lines for this comparison test.
		case "DRY_RUN", "ALERT", "SKIP", "PARSE_FAIL":
			// Exclude informational start/finish messages from the comparison.
			logMessage := fmt.Sprintf(format, args...)
			if strings.HasPrefix(logMessage, "Starting dry-run mode") || strings.HasPrefix(logMessage, "Dry-run finished") {
				return
			}

			// Normalize ALERT and DRY_RUN to a common "ACTION" tag for comparison.
			// This makes the output independent of the mode.
			normalizedTag := tag
			if tag == "DRY_RUN" || tag == "ALERT" {
				normalizedTag = "ACTION"
			}

			msg := fmt.Sprintf(format, args...)
			// Remove the "(DryRun)" suffix to make outputs identical.
			msg = strings.Replace(msg, " (DryRun)", "", 1)
			_, _ = logOutput.Write([]byte(fmt.Sprintf("%s: %s\n", normalizedTag, msg)))
		}
	}

	// Load base configuration from a test file.
	loadedCfg, err := LoadConfigFromYAML(LoadConfigOptions{ConfigPath: "testdata/config.yaml"})
	if err != nil {
		t.Fatalf("Failed to load test YAML config: %v", err)
	}

	// Create a minimal AppConfig for the test.
	// We load the full config to get settings like out_of_order_tolerance and whitelist_cidrs.
	appConfig := &AppConfig{
		CleanupInterval:      loadedCfg.CleanupInterval,
		DefaultBlockDuration: loadedCfg.DefaultBlockDuration,
		EOFPollingDelay:      10 * time.Millisecond, // Keep this fast for testing.
		FileDependencies:     loadedCfg.FileDependencies,
		MaxTimeSinceLastHit:  loadedCfg.MaxTimeSinceLastHit,
		OutOfOrderTolerance:  loadedCfg.OutOfOrderTolerance,
		TimestampFormat:      loadedCfg.TimestampFormat,

		StatFunc: func(path string) (os.FileInfo, error) {
			// For the purpose of this test, we only need to return a mock FileInfo
			// that has a non-nil Sys() value. The actual values don't matter
			// as we are not testing file rotation.
			return &mockFileInfo{
				size: 1024,
				sys:  &syscall.Stat_t{}, // Non-nil value to prevent panic
			}, nil
		},
		FileOpener: func(name string) (fileHandle, error) {
			return os.Open(name)
		},
	}

	// Initialize the processor.
	p := &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		Metrics:       metrics.NewMetrics(),
		ConfigMutex:   &sync.RWMutex{},
		Chains:        loadedCfg.Chains,
		Config:        appConfig,
		LogRegex:      loadedCfg.LogFormatRegex, // Use the regex from the loaded config, which defaults correctly.
		DryRun:        dryRun,
		LogFunc:       logFunc,
		NowFunc:       time.Now,
		LogPath:       logFilePath,
		signalCh:      make(chan os.Signal, 1),
	}

	// Initialize the flush signal channel and function to prevent nil pointer dereference.
	p.oooBufferFlushSignal = make(chan struct{}, 1)
	p.signalOooBufferFlush = p.doSignalOooBufferFlush

	p.CheckChainsFunc = func(entry *LogEntry) { CheckChains(p, entry) }
	p.Blocker = blocker.NewHAProxyBlocker(p, dryRun) // Initialize the blocker to prevent nil pointer panic.

	lineProcessedCh := make(chan struct{}, 100) // Buffered channel to prevent blocking
	p.ProcessLogLine = func(line string) {
		processLogLineInternal(p, line)
		select {
		case lineProcessedCh <- struct{}{}:
		default:
		}
	}

	return p, &logOutput, lineProcessedCh
}

// TestDryRunVsLiveModeComparison compares the behavior of dry-run and live tailing modes.
func TestDryRunVsLiveModeComparison(t *testing.T) {
	// 1. Use the existing test log file.
	logFilePath := "testdata/test_access.log"
	logData, err := os.ReadFile(logFilePath)
	if err != nil {
		t.Fatalf("Failed to read test log file %s: %v", logFilePath, err)
	}

	// 2. Extract expected log output from comments in the test log file.
	expectedLogs := make(map[int]string)
	scanner := bufio.NewScanner(bytes.NewReader(logData))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if strings.Contains(line, "=== EXPECTED LOG:") {
			parts := strings.SplitN(line, "=== EXPECTED LOG:", 2)
			if len(parts) == 2 {
				expected := strings.TrimSpace(parts[1])
				// The actual log line that triggers the output is usually 2 lines after the comment.
				// We store the expected output against the line number where the comment appears.
				// The check logic will handle formatting placeholders like %d.
				expectedLogs[lineNumber] = expected
			}
		}
	}
	if len(expectedLogs) == 0 {
		t.Fatal("No '=== EXPECTED LOG:' comments found in test_access.log")
	}

	// 3. Create a "clean" version of the log data, stripping comments and empty lines.
	// This ensures both modes process the exact same set of meaningful log lines.
	var cleanLogData strings.Builder
	logScanner := bufio.NewScanner(bytes.NewReader(logData))
	for logScanner.Scan() {
		line := logScanner.Text()
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine != "" && !strings.HasPrefix(trimmedLine, "#") {
			cleanLogData.WriteString(line + "\n")
		}
	}

	// --- Create temporary files for both modes with the clean data ---
	createTempLogFile := func(namePrefix string) string {
		file, err := os.CreateTemp(t.TempDir(), namePrefix)
		if err != nil {
			t.Fatalf("Failed to create temp log file: %v", err)
		}
		defer func() {
			_ = file.Close()
		}()
		if _, err := file.WriteString(cleanLogData.String()); err != nil {
			t.Fatalf("Failed to write to temp log file: %v", err)
		}
		absPath, _ := filepath.Abs(file.Name())
		return absPath
	}

	dryRunLogFilePath := createTempLogFile("dry_run_test_*.log")

	// --- Run in Dry-Run Mode ---
	dryRunProcessor, dryRunLogs, _ := setupTestProcessor(t, true, dryRunLogFilePath) // No need for lineProcessedCh in dry-run
	dryRunDone := make(chan struct{})
	go DryRunLogProcessor(dryRunProcessor, dryRunDone)
	<-dryRunDone

	// --- Run in Live Mode ---
	// For live mode, we must simulate new lines being written.
	// 1. Create an EMPTY temp file first.
	liveFile, err := os.CreateTemp(t.TempDir(), "live_tail_test_*.log")
	if err != nil {
		t.Fatalf("Failed to create empty temp file for live mode: %v", err)
	}
	_ = liveFile.Close() // Close it immediately, the tailer will open it.
	liveLogPath, _ := filepath.Abs(liveFile.Name())

	// 2. Setup the processor to watch the empty file.
	liveProcessor, liveLogs, liveLineProcessedCh := setupTestProcessor(t, false, liveLogPath)
	liveSignalCh := make(chan os.Signal, 1)
	liveReadySignal := make(chan struct{}, 1)

	var wg sync.WaitGroup
	wg.Add(1)
	// 3. Start the tailer in a goroutine.
	go func() {
		defer wg.Done()
		LiveLogTailer(liveProcessor, liveSignalCh, liveReadySignal)
	}()

	// 4. Wait for the tailer to be ready (i.e., watching the file).
	<-liveReadySignal

	// 5. NOW write the data to the file, simulating new log lines appearing.
	// We must use os.O_APPEND to correctly simulate new lines being added to a log file.
	// os.WriteFile truncates the file, which the tailer will not detect as new content.
	f, err := os.OpenFile(liveLogPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("Failed to open live tailer log file for appending: %v", err)
	}
	_, _ = f.WriteString(cleanLogData.String())
	_ = f.Close()

	// Wait for all lines to be processed by the live tailer.
	// Count the actual number of lines that should be processed (non-empty, non-comment).
	// This count is based on the cleanLogData, which already strips comments and empty lines.
	numLinesToProcess := strings.Count(cleanLogData.String(), "\n")
	for i := 0; i < numLinesToProcess; i++ {
		select {
		case <-liveLineProcessedCh:
			// Line processed.
		case <-time.After(5 * time.Second): // Generous timeout for processing all lines
			t.Fatalf("Timed out waiting for line %d to be processed by live tailer. Processed so far: %d", i+1, i)
		}
	}

	// Send shutdown signal to the live tailer.
	liveSignalCh <- os.Interrupt
	// Wait for the tailer goroutine to finish completely.
	wg.Wait()

	// 5. Compare the results
	dryRunOutput := dryRunLogs.String()
	liveOutput := liveLogs.String()

	// Filter out OutOfOrderChain lines from both outputs for comparison.
	// Out-of-order handling requires the entryBufferWorker, which adds complexity to this test.
	filterOutOfOrderLines := func(output string) string {
		var filtered []string
		for _, line := range strings.Split(output, "\n") {
			if !strings.Contains(line, "OutOfOrderChain") {
				filtered = append(filtered, line)
			}
		}
		return strings.Join(filtered, "\n")
	}
	dryRunFiltered := filterOutOfOrderLines(dryRunOutput)
	liveFiltered := filterOutOfOrderLines(liveOutput)

	// First, ensure both modes produce identical output (excluding out-of-order chain).
	if dryRunFiltered != liveFiltered {
		t.Errorf("Dry-run and live mode outputs differ (OutOfOrderChain excluded).\n\nDry-run output:\n%s\nLive mode output:\n%s", dryRunFiltered, liveFiltered)
	}

	// Second, verify that the output matches the expectations from the log file comments.
	// We only need to check one of the outputs since we've already confirmed they are identical.
	for commentLine, expectedLog := range expectedLogs {
		// Skip OutOfOrderChain expectations in this test.
		// Out-of-order handling requires the entryBufferWorker to be running, which
		// complicates this comparison test. Out-of-order functionality is tested
		// separately in dedicated buffer worker tests.
		if strings.Contains(expectedLog, "OutOfOrderChain") {
			continue
		}

		found := false
		// Handle placeholders like "Line %d:"
		// The placeholder is no longer used, but we keep the variable for clarity.
		// The check now relies on content, not line number.
		formattedExpectedLog := expectedLog
		formattedExpectedLog = strings.Replace(formattedExpectedLog, "Line %d: ", "", 1)

		// Normalize the expected log for comparison, just like we did in the logFunc.
		normalizedExpected := formattedExpectedLog
		normalizedExpected = strings.Replace(normalizedExpected, "DRY_RUN: ", "ACTION: ", 1)
		normalizedExpected = strings.Replace(normalizedExpected, "ALERT: ", "ACTION: ", 1)
		normalizedExpected = strings.Replace(normalizedExpected, " (DryRun)", "", 1)

		// Now check if the normalized output contains the normalized expected log.
		if strings.Contains(liveOutput, normalizedExpected) {
			found = true
		}

		if !found {
			t.Errorf("Expected log message was not found in live/dry-run output.\n\nEXPECTED (from line %d, normalized):\n'%s'\n\nACTUAL OUTPUT:\n%s",
				commentLine, normalizedExpected, liveOutput)
		}
	}
}
