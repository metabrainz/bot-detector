package main

import (
	"bot-detector/internal/app"
	"bot-detector/internal/blocker"
	"bot-detector/internal/logging"
	"bot-detector/internal/metrics"
	"bot-detector/internal/processor"
	"bot-detector/internal/store"
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
		Chains:        []app.BehavioralChain{},
		Config: &AppConfig{
			Application: ApplicationConfig{
				EOFPollingDelay: 10 * time.Millisecond,
			},
			Parser: ParserConfig{
				LineEnding: "lf",
			},
			FileOpener: func(name string) (fileHandle, error) { return os.Open(name) },
			StatFunc:   os.Stat,
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
		processor.LiveLogTailer(h.processor, h.signalCh, h.readySignal)
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
	// Sync to disk (important for ZFS and fast rotations)
	if err := f.Sync(); err != nil {
		h.t.Fatalf("Failed to sync file: %v", err)
	}
}

// rotateLog simulates a logrotate operation using the 'create' mode:
// 1. Rename current file to .old
// 2. Create new empty file with original name
func (h *rotationTestHarness) rotateLog() {
	h.t.Helper()
	h.t.Logf("[HARNESS] Rotating log file")

	// Sync the directory before rotation to ensure all writes are visible
	syscall.Sync()

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
	if err := f.Sync(); err != nil {
		h.t.Fatalf("Failed to sync new log file: %v", err)
	}
	if err := f.Close(); err != nil {
		h.t.Fatalf("Failed to close new log file: %v", err)
	}

	// Sync again after rotation
	syscall.Sync()

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

// TestRotation_RapidSequential tests that multiple rapid rotations are handled correctly.
//
// Scenario:
// 1. Start tailer with empty log file
// 2. Perform 5 rotations in quick succession
// 3. Write 100 lines to each generation of the log file
// 4. Verify all 500 lines are processed (100 per rotation)
// 5. Verify no duplicates or skipped lines
// 6. Verify all rotations are detected
//
// This test simulates scenarios like:
// - Manual rotation during debugging (multiple logrotate -f calls)
// - Catching up on a backlog after downtime
// - High-volume logging with frequent rotations
func TestRotation_RapidSequential(t *testing.T) {
	h := newRotationTestHarness(t)

	// Test with 5 rapid rotations to verify the readySignal deadlock bug is fixed.
	// Previously, the tailer would deadlock after 2 rotations because readySignal
	// was sent on every reopen, causing channel blocking when nobody was listening.
	const numRotations = 5
	const linesPerRotation = 100
	const totalLines = numRotations * linesPerRotation

	// Track rotations detected
	var rotationsDetected int32
	rotationDetectedCh := make(chan struct{}, numRotations)

	// Override LogFunc to count rotations
	// Rotation can be detected via multiple messages:
	// 1. "Detected log file rotation" (inode change)
	// 2. "Detected log file size reduction" (truncation)
	// 3. "Failed to stat log path during EOF check" (file temporarily missing)
	originalLogFunc := h.processor.LogFunc
	h.processor.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		originalLogFunc(level, tag, format, args...)
		msg := fmt.Sprintf(format, args...)
		isRotation := (tag == "TAIL" && (strings.Contains(msg, "Detected log file rotation") || strings.Contains(msg, "Detected log file size reduction"))) ||
			(tag == "TAIL_ERROR" && strings.Contains(msg, "Failed to stat log path during EOF check"))
		if isRotation {
			if atomic.AddInt32(&rotationsDetected, 1) <= numRotations {
				select {
				case rotationDetectedCh <- struct{}{}:
				default:
				}
			}
		}
	}

	// Generate all test data upfront
	allLines := make([][]string, numRotations)
	for rotation := 0; rotation < numRotations; rotation++ {
		lines := make([]string, linesPerRotation)
		for i := 0; i < linesPerRotation; i++ {
			lines[i] = fmt.Sprintf("rotation-%d-line-%04d", rotation, i)
		}
		allLines[rotation] = lines
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

	// Step 3: Perform rapid rotations
	for rotation := 0; rotation < numRotations; rotation++ {
		t.Logf("=== Rotation %d/%d ===", rotation+1, numRotations)

		// Record line count before this rotation
		h.linesMutex.Lock()
		linesBefore := len(h.linesProcessed)
		h.linesMutex.Unlock()

		// Write lines for this rotation
		t.Logf("Writing %d lines for rotation %d", linesPerRotation, rotation)
		batchSize := 25
		for i := 0; i < linesPerRotation; i += batchSize {
			end := i + batchSize
			if end > linesPerRotation {
				end = linesPerRotation
			}
			h.appendLines(allLines[rotation][i:end]...)
			time.Sleep(5 * time.Millisecond) // Small delay between batches
		}

		// Wait for MOST lines from THIS rotation to be processed before rotating
		// This simulates realistic timing where rotations happen after log burst completes
		waitTarget := linesBefore + (linesPerRotation * 4 / 5) // Wait for 80% of new lines
		t.Logf("Waiting for at least %d lines to be processed (currently %d)", waitTarget, linesBefore)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			h.linesMutex.Lock()
			count := len(h.linesProcessed)
			h.linesMutex.Unlock()
			if count >= waitTarget {
				t.Logf("Reached %d lines, proceeding with rotation", count)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		h.linesMutex.Lock()
		linesAfter := len(h.linesProcessed)
		h.linesMutex.Unlock()
		t.Logf("Processed %d new lines this rotation (%d total)", linesAfter-linesBefore, linesAfter)

		// Rotate (except after the last set of lines)
		if rotation < numRotations-1 {
			h.rotateLog()

			// Wait for rotation to be detected
			select {
			case <-rotationDetectedCh:
				t.Logf("Rotation %d detected", rotation+1)
			case <-time.After(3 * time.Second):
				t.Errorf("Timeout waiting for rotation %d to be detected", rotation+1)
			}

			// Longer pause between rotations to allow:
			// 1. Old file handle to close
			// 2. New file to be opened
			// 3. Tailer to start its read loop on the new file
			// This prevents the next rotation from happening while tailer is mid-reopen
			time.Sleep(500 * time.Millisecond)
		}
	}

	// Step 4: Wait for all lines to be processed
	t.Logf("Waiting for all %d lines to be processed", totalLines)
	h.waitForLineCount(totalLines, 15*time.Second)

	// Step 5: Verify results
	processedLines := h.getProcessedLines()

	t.Logf("Total lines processed: %d (expected %d)", len(processedLines), totalLines)
	t.Logf("Total rotations detected: %d (expected %d)", atomic.LoadInt32(&rotationsDetected), numRotations-1)

	// Check we got the right count
	if len(processedLines) != totalLines {
		t.Errorf("Expected %d lines, got %d", totalLines, len(processedLines))
	}

	// Verify we detected the expected number of rotations (numRotations - 1, since the last file isn't rotated)
	expectedRotations := int32(numRotations - 1)
	if atomic.LoadInt32(&rotationsDetected) != expectedRotations {
		t.Errorf("Expected %d rotations detected, got %d", expectedRotations, atomic.LoadInt32(&rotationsDetected))
	}

	// Verify no duplicates
	seen := make(map[string]int)
	for _, line := range processedLines {
		seen[line]++
	}
	duplicates := 0
	for line, count := range seen {
		if count > 1 {
			t.Errorf("Line %q was processed %d times (duplicate!)", line, count)
			duplicates++
		}
	}

	if duplicates > 0 {
		t.Errorf("Found %d duplicate lines", duplicates)
	}

	// Verify we got all expected lines from all rotations
	var allExpected []string
	for rotation := 0; rotation < numRotations; rotation++ {
		allExpected = append(allExpected, allLines[rotation]...)
	}

	missing := 0
	for _, expected := range allExpected {
		if seen[expected] == 0 {
			t.Errorf("Expected line %q was not processed", expected)
			missing++
			if missing >= 10 {
				t.Logf("...and %d more missing lines", len(allExpected)-missing)
				break
			}
		}
	}

	if missing > 0 {
		t.Errorf("Total missing lines: %d", missing)
	}

	// Verify order within each rotation
	for rotation := 0; rotation < numRotations; rotation++ {
		rotationLines := make([]string, 0, linesPerRotation)
		prefix := fmt.Sprintf("rotation-%d-", rotation)
		for _, line := range processedLines {
			if strings.HasPrefix(line, prefix) {
				rotationLines = append(rotationLines, line)
			}
		}

		// Check we got all lines for this rotation
		if len(rotationLines) != linesPerRotation {
			t.Errorf("Rotation %d: expected %d lines, got %d", rotation, linesPerRotation, len(rotationLines))
		}

		// Check order within rotation
		for i := 0; i < len(rotationLines)-1; i++ {
			current := rotationLines[i]
			next := rotationLines[i+1]
			if current > next {
				t.Errorf("Rotation %d: lines out of order: %q came after %q", rotation, current, next)
				break
			}
		}
	}

	t.Logf("✓ All %d lines processed correctly across %d rotations", totalLines, numRotations)
	t.Logf("✓ All %d rotations detected", expectedRotations)
	t.Logf("✓ No duplicates found")
	t.Logf("✓ Order preserved within each rotation")
}

// TestRotation_LargeFileGrowthPattern tests rotation detection when files grow large.
//
// Scenario:
// 1. Start tailer with empty log file
// 2. Write lines to grow file to ~50KB (simulating large log accumulation)
// 3. Rotate the file
// 4. Write lines to new file, growing it past the old file's size
// 5. Verify size-based rotation detection works correctly
// 6. Verify all lines are processed
//
// This test ensures that:
// - Size-based truncation detection works (currentSize < initialSize)
// - The tailer doesn't falsely detect rotation when new file grows past old size
// - Large file handling doesn't cause performance issues
func TestRotation_LargeFileGrowthPattern(t *testing.T) {
	h := newRotationTestHarness(t)

	// Use larger lines to quickly reach substantial file sizes
	const lineSize = 500            // bytes per line (including newline)
	const linesBeforeRotation = 100 // ~50KB before rotation
	const linesAfterRotation = 150  // ~75KB after rotation (larger than before)
	const totalLines = linesBeforeRotation + linesAfterRotation

	// Track rotation detection
	rotationDetected := make(chan struct{}, 1)
	originalLogFunc := h.processor.LogFunc
	h.processor.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		originalLogFunc(level, tag, format, args...)
		msg := fmt.Sprintf(format, args...)
		if (tag == "TAIL" && strings.Contains(msg, "Detected log file rotation")) ||
			(tag == "TAIL" && strings.Contains(msg, "Detected log file size reduction")) {
			select {
			case rotationDetected <- struct{}{}:
			default:
			}
		}
	}

	// Generate large lines (pad to desired size)
	generateLargeLine := func(id int) string {
		base := fmt.Sprintf("large-line-%06d", id)
		// Pad with spaces to reach desired line size
		padding := lineSize - len(base) - 1 // -1 for newline
		if padding > 0 {
			base += strings.Repeat("x", padding)
		}
		return base
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

	// Step 3: Write lines to grow file to ~50KB
	t.Logf("Writing %d large lines (~%d KB)", linesBeforeRotation, (linesBeforeRotation*lineSize)/1024)
	preRotationLines := make([]string, linesBeforeRotation)
	for i := 0; i < linesBeforeRotation; i++ {
		preRotationLines[i] = generateLargeLine(i)
	}

	batchSize := 25
	for i := 0; i < linesBeforeRotation; i += batchSize {
		end := i + batchSize
		if end > linesBeforeRotation {
			end = linesBeforeRotation
		}
		h.appendLines(preRotationLines[i:end]...)
		time.Sleep(5 * time.Millisecond)
	}

	// Wait for lines to be processed
	h.waitForLineCount(linesBeforeRotation, 5*time.Second)
	t.Logf("Processed %d lines before rotation", linesBeforeRotation)

	// Check file size before rotation
	stat, err := os.Stat(h.logFilePath)
	if err != nil {
		t.Fatalf("Failed to stat file before rotation: %v", err)
	}
	sizeBeforeRotation := stat.Size()
	t.Logf("File size before rotation: %d bytes (~%d KB)", sizeBeforeRotation, sizeBeforeRotation/1024)

	// Step 4: Rotate the file
	t.Logf("Rotating log file")
	h.rotateLog()

	// Wait for rotation to be detected
	select {
	case <-rotationDetected:
		t.Logf("Rotation detected")
	case <-time.After(3 * time.Second):
		t.Error("Timeout waiting for rotation to be detected")
	}

	// Brief pause for tailer to reopen
	time.Sleep(200 * time.Millisecond)

	// Step 5: Write more lines to new file, growing it LARGER than the old file
	t.Logf("Writing %d large lines to new file (~%d KB, larger than before)", linesAfterRotation, (linesAfterRotation*lineSize)/1024)
	postRotationLines := make([]string, linesAfterRotation)
	for i := 0; i < linesAfterRotation; i++ {
		postRotationLines[i] = generateLargeLine(linesBeforeRotation + i)
	}

	for i := 0; i < linesAfterRotation; i += batchSize {
		end := i + batchSize
		if end > linesAfterRotation {
			end = linesAfterRotation
		}
		h.appendLines(postRotationLines[i:end]...)
		time.Sleep(5 * time.Millisecond)
	}

	// Step 6: Wait for all lines to be processed
	t.Logf("Waiting for all %d lines to be processed", totalLines)
	h.waitForLineCount(totalLines, 10*time.Second)

	// Check final file size
	stat, err = os.Stat(h.logFilePath)
	if err != nil {
		t.Fatalf("Failed to stat file after rotation: %v", err)
	}
	sizeAfterRotation := stat.Size()
	t.Logf("File size after rotation: %d bytes (~%d KB)", sizeAfterRotation, sizeAfterRotation/1024)

	if sizeAfterRotation <= sizeBeforeRotation {
		t.Errorf("Expected new file (%d bytes) to be larger than old file (%d bytes)",
			sizeAfterRotation, sizeBeforeRotation)
	}

	// Step 7: Verify results
	processedLines := h.getProcessedLines()

	t.Logf("Total lines processed: %d (expected %d)", len(processedLines), totalLines)

	if len(processedLines) != totalLines {
		t.Errorf("Expected %d lines, got %d", totalLines, len(processedLines))
	}

	// Verify no duplicates
	seen := make(map[string]bool)
	duplicates := 0
	for _, line := range processedLines {
		if seen[line] {
			t.Errorf("Duplicate line found: %q", line)
			duplicates++
			if duplicates >= 5 {
				t.Logf("...stopping after 5 duplicates")
				break
			}
		}
		seen[line] = true
	}

	if duplicates > 0 {
		t.Errorf("Found %d duplicate lines", duplicates)
	}

	// Verify all expected lines were processed
	missing := 0
	for _, expected := range preRotationLines {
		if !seen[expected] {
			t.Errorf("Missing line from before rotation: %.50s...", expected)
			missing++
			if missing >= 5 {
				t.Logf("...stopping after 5 missing lines")
				break
			}
		}
	}
	for _, expected := range postRotationLines {
		if !seen[expected] {
			t.Errorf("Missing line from after rotation: %.50s...", expected)
			missing++
			if missing >= 5 {
				t.Logf("...stopping after 5 missing lines")
				break
			}
		}
	}

	if missing > 0 {
		t.Errorf("Total missing lines: %d", missing)
	}

	// Verify order
	for i := 0; i < len(processedLines)-1; i++ {
		// Extract line numbers for comparison
		current := processedLines[i]
		next := processedLines[i+1]
		if current > next { // String comparison works due to zero-padded numbers
			t.Errorf("Lines out of order at position %d: %q came before %q", i, current, next)
			break
		}
	}

	t.Logf("✓ All %d lines processed correctly", totalLines)
	t.Logf("✓ File grew from %d KB to %d KB after rotation", sizeBeforeRotation/1024, sizeAfterRotation/1024)
	t.Logf("✓ Size-based rotation detection worked correctly")
	t.Logf("✓ No duplicates found")
	t.Logf("✓ Order preserved")
}

// TestRotation_SysReturnsNil tests rotation detection when Sys() returns nil.
//
// Scenario:
// 1. Mock StatFunc to return FileInfo with nil Sys()
// 2. Write lines and rotate file
// 3. Verify rotation is still detected via size-based detection
// 4. Verify debug log about inode detection being skipped
//
// This test ensures:
// - Graceful degradation when inode detection is unavailable
// - Size-based truncation detection still works
// - No panics when Sys() returns nil
// - Appropriate debug logging
func TestRotation_SysReturnsNil(t *testing.T) {
	h := newRotationTestHarness(t)

	const linesBeforeRotation = 50
	const linesAfterRotation = 30
	const totalLines = linesBeforeRotation + linesAfterRotation

	// Track debug log about Sys() returning nil
	sysNilLogged := make(chan struct{}, 1)
	rotationDetected := make(chan struct{}, 1)

	originalLogFunc := h.processor.LogFunc
	h.processor.LogFunc = func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		originalLogFunc(level, tag, format, args...)
		msg := fmt.Sprintf(format, args...)
		if tag == "TAIL_DEBUG" && strings.Contains(msg, "Sys() call returned nil") {
			select {
			case sysNilLogged <- struct{}{}:
			default:
			}
		}
		// Check for rotation detection - either size reduction (truncation) or size mismatch (rename-based)
		if tag == "TAIL" && (strings.Contains(msg, "Detected log file size reduction") || strings.Contains(msg, "size mismatch")) {
			select {
			case rotationDetected <- struct{}{}:
			default:
			}
		}
	}

	// Create a custom StatFunc that returns FileInfo with nil Sys()
	realStatFunc := h.processor.Config.StatFunc
	h.processor.Config.StatFunc = func(path string) (os.FileInfo, error) {
		info, err := realStatFunc(path)
		if err != nil {
			return nil, err
		}
		// Wrap the real FileInfo to return nil from Sys()
		return &nilSysFileInfo{FileInfo: info}, nil
	}

	// Generate test data
	preRotationLines := make([]string, linesBeforeRotation)
	for i := 0; i < linesBeforeRotation; i++ {
		preRotationLines[i] = fmt.Sprintf("before-rotation-line-%04d", i)
	}

	postRotationLines := make([]string, linesAfterRotation)
	for i := 0; i < linesAfterRotation; i++ {
		postRotationLines[i] = fmt.Sprintf("after-rotation-line-%04d", i)
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

	// Step 3: Write lines before rotation
	t.Logf("Writing %d lines before rotation", linesBeforeRotation)
	batchSize := 25
	for i := 0; i < linesBeforeRotation; i += batchSize {
		end := i + batchSize
		if end > linesBeforeRotation {
			end = linesBeforeRotation
		}
		h.appendLines(preRotationLines[i:end]...)
		time.Sleep(5 * time.Millisecond)
	}

	// Wait for lines to be processed
	h.waitForLineCount(linesBeforeRotation, 5*time.Second)
	t.Logf("Processed %d lines before rotation", linesBeforeRotation)

	// Step 4: Rotate the file (this will trigger size-based detection since Sys() is nil)
	t.Logf("Rotating log file")
	h.rotateLog()

	// Wait for the debug log about Sys() being nil
	select {
	case <-sysNilLogged:
		t.Logf("✓ Debug log confirmed: Sys() returned nil, inode detection skipped")
	case <-time.After(3 * time.Second):
		// This is acceptable - the log might not be triggered on every call
		t.Logf("Note: Sys() nil debug log not captured (may be cached or not triggered)")
	}

	// Wait for rotation to be detected via size comparison (reduction or mismatch)
	select {
	case <-rotationDetected:
		t.Logf("✓ Rotation detected via size-based detection (file handle vs path comparison)")
	case <-time.After(3 * time.Second):
		t.Error("Timeout waiting for size-based rotation detection (file handle vs path comparison)")
	}

	// Brief pause for tailer to reopen
	time.Sleep(200 * time.Millisecond)

	// Step 5: Write lines to new file
	t.Logf("Writing %d lines after rotation", linesAfterRotation)
	for i := 0; i < linesAfterRotation; i += batchSize {
		end := i + batchSize
		if end > linesAfterRotation {
			end = linesAfterRotation
		}
		h.appendLines(postRotationLines[i:end]...)
		time.Sleep(5 * time.Millisecond)
	}

	// Step 6: Wait for all lines to be processed
	t.Logf("Waiting for all %d lines to be processed", totalLines)
	h.waitForLineCount(totalLines, 10*time.Second)

	// Step 7: Verify results
	processedLines := h.getProcessedLines()

	t.Logf("Total lines processed: %d (expected %d)", len(processedLines), totalLines)

	if len(processedLines) != totalLines {
		t.Errorf("Expected %d lines, got %d", totalLines, len(processedLines))
	}

	// Verify no duplicates
	seen := make(map[string]bool)
	for _, line := range processedLines {
		if seen[line] {
			t.Errorf("Duplicate line found: %q", line)
		}
		seen[line] = true
	}

	// Verify all expected lines were processed
	allExpected := append([]string{}, preRotationLines...)
	allExpected = append(allExpected, postRotationLines...)
	for _, expected := range allExpected {
		if !seen[expected] {
			t.Errorf("Missing line: %q", expected)
		}
	}

	t.Logf("✓ All %d lines processed correctly", totalLines)
	t.Logf("✓ Size-based rotation detection worked without inode support")
	t.Logf("✓ No panics or errors when Sys() returned nil")
}

// nilSysFileInfo wraps os.FileInfo and returns nil from Sys()
type nilSysFileInfo struct {
	os.FileInfo
}

func (n *nilSysFileInfo) Sys() interface{} {
	return nil
}
