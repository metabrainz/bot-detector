package main

import (
	"bot-detector/internal/blocker"
	"bot-detector/internal/logging"
	"bot-detector/internal/metrics"
	"bot-detector/internal/store"
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// rotationTestHarness provides a complete environment for testing log rotation scenarios.
type rotationTestHarness struct {
	t              *testing.T
	tempDir        string
	logFilePath    string
	processor      *Processor
	signalCh       chan os.Signal
	readySignal    chan struct{}
	doneCh         chan struct{}
	linesProcessed []string // Track all lines processed in order
	linesMutex     sync.Mutex
	wg             sync.WaitGroup
}

// newRotationTestHarness creates a new test harness for rotation testing.
func newRotationTestHarness(t *testing.T) *rotationTestHarness {
	t.Helper()

	tempDir := t.TempDir()
	logFilePath := filepath.Join(tempDir, "test.log")

	h := &rotationTestHarness{
		t:              t,
		tempDir:        tempDir,
		logFilePath:    logFilePath,
		signalCh:       make(chan os.Signal, 1),
		readySignal:    make(chan struct{}, 1),
		doneCh:         make(chan struct{}),
		linesProcessed: make([]string, 0),
	}

	// Create processor with realistic configuration
	h.processor = &Processor{
		ActivityMutex: &sync.RWMutex{},
		ActivityStore: make(map[store.Actor]*store.ActorActivity),
		Metrics:       metrics.NewMetrics(),
		ConfigMutex:   &sync.RWMutex{},
		Chains:        []BehavioralChain{},
		Config: &AppConfig{
			EOFPollingDelay: 10 * time.Millisecond,
			LineEnding:      "lf",
			FileOpener:      func(name string) (fileHandle, error) { return os.Open(name) },
			StatFunc:        os.Stat,
		},
		DryRun:  false,
		NowFunc: time.Now,
		LogPath: logFilePath,
	}

	// Initialize blocker to prevent nil pointer issues
	h.processor.Blocker = blocker.NewHAProxyBlocker(h.processor, false)

	// Initialize out-of-order buffer infrastructure
	h.processor.oooBufferFlushSignal = make(chan struct{}, 1)
	h.processor.signalOooBufferFlush = h.processor.doSignalOooBufferFlush

	// Set up logging that captures output for debugging
	h.processor.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		msg := fmt.Sprintf("[%s] "+format, append([]interface{}{tag}, args...)...)
		t.Logf("%s", msg)
	}

	// Track all processed lines
	h.processor.ProcessLogLine = func(line string) {
		h.linesMutex.Lock()
		defer h.linesMutex.Unlock()
		h.linesProcessed = append(h.linesProcessed, line)
	}

	return h
}

// start launches the live tailer in a background goroutine.
func (h *rotationTestHarness) start() {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer close(h.doneCh)
		LiveLogTailer(h.processor, h.signalCh, h.readySignal)
	}()

	// Wait for tailer to be ready
	select {
	case <-h.readySignal:
		h.t.Logf("[HARNESS] Tailer is ready")
	case <-time.After(2 * time.Second):
		h.t.Fatal("[HARNESS] Timed out waiting for tailer to be ready")
	}
}

// stop gracefully shuts down the tailer.
func (h *rotationTestHarness) stop() {
	h.t.Logf("[HARNESS] Stopping tailer")
	h.signalCh <- syscall.SIGTERM
	select {
	case <-h.doneCh:
		h.t.Logf("[HARNESS] Tailer stopped")
	case <-time.After(2 * time.Second):
		h.t.Fatal("[HARNESS] Timed out waiting for tailer to stop")
	}
	h.wg.Wait()
}

// appendLines appends lines to the current log file.
func (h *rotationTestHarness) appendLines(lines ...string) {
	h.t.Helper()
	f, err := os.OpenFile(h.logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		h.t.Fatalf("Failed to open log file for appending: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			h.t.Logf("Warning: failed to close file: %v", err)
		}
	}()

	w := bufio.NewWriter(f)
	for _, line := range lines {
		if _, err := w.WriteString(line + "\n"); err != nil {
			h.t.Fatalf("Failed to write line: %v", err)
		}
	}
	if err := w.Flush(); err != nil {
		h.t.Fatalf("Failed to flush writer: %v", err)
	}
}

// rotateLog simulates a logrotate operation using the 'create' mode:
// 1. Rename current file to .old
// 2. Create new empty file with original name
func (h *rotationTestHarness) rotateLog() {
	h.t.Helper()
	h.t.Logf("[HARNESS] Rotating log file")

	// Rename old file
	oldPath := h.logFilePath + ".old"
	if err := os.Rename(h.logFilePath, oldPath); err != nil {
		h.t.Fatalf("Failed to rename log file during rotation: %v", err)
	}

	// Create new empty file
	f, err := os.Create(h.logFilePath)
	if err != nil {
		h.t.Fatalf("Failed to create new log file during rotation: %v", err)
	}
	if err := f.Close(); err != nil {
		h.t.Fatalf("Failed to close new log file: %v", err)
	}

	h.t.Logf("[HARNESS] Log rotation complete")
}

// waitForLineCount waits until the expected number of lines have been processed.
func (h *rotationTestHarness) waitForLineCount(expected int, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		h.linesMutex.Lock()
		count := len(h.linesProcessed)
		h.linesMutex.Unlock()

		if count >= expected {
			h.t.Logf("[HARNESS] Processed %d/%d lines", count, expected)
			return
		}

		select {
		case <-ticker.C:
			continue
		case <-time.After(time.Until(deadline)):
			h.linesMutex.Lock()
			count = len(h.linesProcessed)
			h.linesMutex.Unlock()
			h.t.Fatalf("Timeout waiting for %d lines, only got %d", expected, count)
		}
	}
}

// getProcessedLines returns a copy of all processed lines.
func (h *rotationTestHarness) getProcessedLines() []string {
	h.linesMutex.Lock()
	defer h.linesMutex.Unlock()
	result := make([]string, len(h.linesProcessed))
	copy(result, h.linesProcessed)
	return result
}

// TestRotation_DuringActiveReading tests that rotation works correctly when
// the log file is actively being written to (simulating real-world scenario).
//
// Scenario:
// 1. Create empty log file and start tailer
// 2. Write lines continuously while tailer is running
// 3. Trigger rotation mid-stream (after ~250 lines)
// 4. Continue writing lines to the NEW file
// 5. Verify all lines are processed in order
// 6. Verify no duplicates or skipped lines
func TestRotation_DuringActiveReading(t *testing.T) {
	h := newRotationTestHarness(t)

	// Generate test data
	const linesBeforeRotation = 500
	const linesAfterRotation = 500
	const totalLines = linesBeforeRotation + linesAfterRotation

	var preRotationLines []string
	for i := 1; i <= linesBeforeRotation; i++ {
		preRotationLines = append(preRotationLines, fmt.Sprintf("line-%04d-before-rotation", i))
	}

	var postRotationLines []string
	for i := 1; i <= linesAfterRotation; i++ {
		postRotationLines = append(postRotationLines, fmt.Sprintf("line-%04d-after-rotation", i))
	}

	// Step 1: Create empty log file
	f, err := os.Create(h.logFilePath)
	if err != nil {
		t.Fatalf("Failed to create log file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Failed to close log file: %v", err)
	}

	// Step 2: Start the tailer
	h.start()
	defer h.stop()

	// Step 3: Start writing lines in batches to simulate active logging
	t.Logf("Writing %d lines before rotation (in batches)", linesBeforeRotation)
	batchSize := 50
	for i := 0; i < linesBeforeRotation; i += batchSize {
		end := i + batchSize
		if end > linesBeforeRotation {
			end = linesBeforeRotation
		}
		h.appendLines(preRotationLines[i:end]...)
		time.Sleep(5 * time.Millisecond) // Small delay between batches
	}

	// Step 4: Wait for approximately half the lines to be processed
	// This ensures we're in the middle of reading when rotation happens
	midPoint := linesBeforeRotation / 2
	h.waitForLineCount(midPoint, 5*time.Second)
	t.Logf("Reached midpoint (%d lines), triggering rotation", midPoint)

	// Step 5: Rotate the log file while there are still unread lines in the old file
	h.rotateLog()

	// Give the tailer a moment to detect rotation and reopen
	time.Sleep(100 * time.Millisecond)

	// Step 6: Write new lines to the NEW file in batches
	t.Logf("Writing %d lines after rotation (in batches)", linesAfterRotation)
	for i := 0; i < linesAfterRotation; i += batchSize {
		end := i + batchSize
		if end > linesAfterRotation {
			end = linesAfterRotation
		}
		h.appendLines(postRotationLines[i:end]...)
		time.Sleep(5 * time.Millisecond)
	}

	// Step 7: Wait for all lines to be processed
	t.Logf("Waiting for all %d lines to be processed", totalLines)
	h.waitForLineCount(totalLines, 15*time.Second)

	// Step 8: Verify results
	processedLines := h.getProcessedLines()

	t.Logf("Total lines processed: %d (expected %d)", len(processedLines), totalLines)

	// Check we got the right count
	if len(processedLines) != totalLines {
		t.Errorf("Expected %d lines, got %d", totalLines, len(processedLines))
	}

	// Verify no duplicates
	seen := make(map[string]int)
	for _, line := range processedLines {
		seen[line]++
	}
	for line, count := range seen {
		if count > 1 {
			t.Errorf("Line %q was processed %d times (duplicate!)", line, count)
		}
	}

	// Verify we got all expected lines (check both before and after rotation)
	allExpected := append([]string{}, preRotationLines...)
	allExpected = append(allExpected, postRotationLines...)

	for _, expected := range allExpected {
		if seen[expected] == 0 {
			t.Errorf("Expected line %q was not processed", expected)
		}
	}

	// Verify order within each rotation segment
	// Note: We can't guarantee perfect order across rotation boundary due to async nature,
	// but lines within each file should maintain order
	beforeLines := make([]string, 0)
	afterLines := make([]string, 0)
	for _, line := range processedLines {
		if strings.Contains(line, "before-rotation") {
			beforeLines = append(beforeLines, line)
		} else if strings.Contains(line, "after-rotation") {
			afterLines = append(afterLines, line)
		}
	}

	// Check order of lines from original file
	for i := 0; i < len(beforeLines)-1; i++ {
		current := beforeLines[i]
		next := beforeLines[i+1]
		if current > next { // String comparison works due to zero-padded numbers
			t.Errorf("Lines from original file out of order: %q came after %q", current, next)
		}
	}

	// Check order of lines from new file
	for i := 0; i < len(afterLines)-1; i++ {
		current := afterLines[i]
		next := afterLines[i+1]
		if current > next {
			t.Errorf("Lines from new file out of order: %q came after %q", current, next)
		}
	}

	t.Logf("✓ All %d lines processed correctly", totalLines)
	t.Logf("✓ No duplicates found")
	t.Logf("✓ Order preserved within each file")
}
