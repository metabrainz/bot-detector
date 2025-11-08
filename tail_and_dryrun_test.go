package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestReadLineWithLimit(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		limit         int
		expectedLine  string
		expectedError error
	}{
		{
			name:          "Line within limit",
			input:         "hello world\n",
			limit:         100,
			expectedLine:  "hello world",
			expectedError: nil,
		},
		{
			name:          "Line at limit",
			input:         "1234567890\n",
			limit:         10,
			expectedLine:  "1234567890",
			expectedError: nil,
		},
		{
			name:          "Line one byte over limit",
			input:         "12345678901\n",
			limit:         10,
			expectedLine:  "1234567890",
			expectedError: ErrLineSkipped,
		},
		{
			name:          "Line exceeds limit",
			input:         "this line is too long\n",
			limit:         10,
			expectedLine:  "this line ",
			expectedError: ErrLineSkipped,
		},
		{
			name:          "EOF without newline",
			input:         "eof",
			limit:         100,
			expectedLine:  "eof",
			expectedError: io.EOF, // Correctly expect EOF
		},
		{
			name:          "Empty input",
			input:         "",
			limit:         100,
			expectedLine:  "",
			expectedError: io.EOF,
		},
		{
			name:          "Windows EOL (CRLF)",
			input:         "windows line\r\n",
			limit:         100,
			expectedLine:  "windows line",
			expectedError: nil,
		},
		{
			name:          "Windows EOL over limit",
			input:         "this is a long windows line\r\n",
			limit:         10,
			expectedLine:  "this is a ",
			expectedError: ErrLineSkipped,
		},
		{
			name:          "Classic Mac EOL (CR)",
			input:         "mac line\rnext line",
			limit:         100,
			expectedLine:  "mac line",
			expectedError: nil,
		},
		{
			name:          "Classic Mac EOL over limit",
			input:         "this is a long mac line\rnext line",
			limit:         10,
			expectedLine:  "this is a ",
			expectedError: ErrLineSkipped,
		},
		{
			name:          "Mixed EOLs (Windows then Unix)",
			input:         "line1\r\nline2\n",
			limit:         100,
			expectedLine:  "line1",
			expectedError: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tt.input))
			line, err := ReadLineWithLimit(reader, tt.limit)

			if line != tt.expectedLine {
				t.Errorf("Line content mismatch. Expected '%s', got '%s'", tt.expectedLine, line)
			}

			if !errors.Is(err, tt.expectedError) {
				t.Errorf("Expected error '%v', got '%v'", tt.expectedError, err)
			}
		})
	}
}

// --- Mocks for testing hasFileBeenRotated ---

// mockFileInfo implements os.FileInfo for testing purposes.
type mockFileInfo struct {
	size int64
	sys  interface{}
}

func (m *mockFileInfo) Name() string       { return "mock.log" }
func (m *mockFileInfo) Size() int64        { return m.size }
func (m *mockFileInfo) Mode() os.FileMode  { return 0644 }
func (m *mockFileInfo) ModTime() time.Time { return time.Now() }
func (m *mockFileInfo) IsDir() bool        { return false }
func (m *mockFileInfo) Sys() interface{}   { return m.sys }

func TestHasFileBeenRotated(t *testing.T) {
	// --- Setup ---
	processor := &Processor{
		LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {}, // No-op logger
	}

	// Initial file state
	initialStat := &mockFileInfo{
		size: 1024,
		sys: &syscall.Stat_t{
			Dev: 1,
			Ino: 12345,
		},
	}

	tests := []struct {
		name         string
		mockStatFunc func(path string) (os.FileInfo, error) // Mocks os.Stat
		expected     bool
		initialStat  os.FileInfo
	}{
		{
			name: "No Rotation or Truncation",
			mockStatFunc: func(path string) (os.FileInfo, error) {
				return &mockFileInfo{size: 2048, sys: &syscall.Stat_t{Dev: 1, Ino: 12345}}, nil
			},
			expected:    false,
			initialStat: initialStat,
		},
		{
			name: "File Rotated (Inode Changed)",
			mockStatFunc: func(path string) (os.FileInfo, error) {
				return &mockFileInfo{size: 512, sys: &syscall.Stat_t{Dev: 1, Ino: 67890}}, nil
			},
			expected:    true,
			initialStat: initialStat,
		},
		{
			name: "File Truncated (Size Decreased)",
			mockStatFunc: func(path string) (os.FileInfo, error) {
				return &mockFileInfo{size: 512, sys: &syscall.Stat_t{Dev: 1, Ino: 12345}}, nil
			},
			expected:    true,
			initialStat: initialStat,
		},
		{
			name: "Stat Fails",
			mockStatFunc: func(path string) (os.FileInfo, error) {
				return nil, os.ErrNotExist
			},
			expected:    true,
			initialStat: initialStat,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --- Act ---
			// We can't directly mock os.Stat, so we pass the result of our mock function.
			// The logic inside hasFileBeenRotated is what we're testing.
			result := hasFileBeenRotated(processor, "dummy/path", tt.initialStat, tt.mockStatFunc)

			// --- Assert ---
			if result != tt.expected {
				t.Errorf("Expected hasFileBeenRotated to return %v, but got %v", tt.expected, result)
			}
		})
	}
}

func TestDryRunLogProcessor(t *testing.T) {
	// --- Setup ---
	// Create a temporary log file for the test.
	tempDir := t.TempDir()
	tempLogFile := filepath.Join(tempDir, "test_dryrun.log")

	// Point the global TestLogPath to our temp file for the duration of the test.
	originalTestLogPath := TestLogPath
	TestLogPath = tempLogFile
	t.Cleanup(func() { TestLogPath = originalTestLogPath })

	// --- Test Cases ---
	tests := []struct {
		name                   string
		logContent             string
		setupFunc              func() // For setup specific to a test case, like file existence.
		expectedLinesProcessed int
		expectedLogContains    string
	}{
		{
			name: "Successful Processing",
			logContent: `line 1
line 2
# a comment
line 3`,
			setupFunc: func() {
				os.WriteFile(tempLogFile, []byte(`line 1
line 2
# a comment
line 3`), 0644)
			},
			expectedLinesProcessed: 3,
			expectedLogContains:    "DryRun complete. Processed 3 lines.",
		},
		{
			name: "File Not Found",
			setupFunc: func() {
				os.Remove(tempLogFile) // Ensure file does not exist.
			},
			expectedLinesProcessed: 0,
			expectedLogContains:    "Failed to open test log file",
		},
		{
			name:       "Line Exceeds Limit",
			logContent: "this is a normal line\n" + strings.Repeat("a", MaxLogLineSize+1) + "\nthis is another normal line",
			setupFunc: func() {
				os.WriteFile(tempLogFile, []byte("this is a normal line\n"+strings.Repeat("a", MaxLogLineSize+1)+"\nthis is another normal line"), 0644)
			},
			expectedLinesProcessed: 2, // The long line is skipped, but the other two are processed.
			expectedLogContains:    "Skipped (Length exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --- Per-Test Setup ---
			tt.setupFunc()

			var linesProcessed int
			var logMutex sync.Mutex
			var capturedLogs []string

			processor := &Processor{
				// Mock ProcessLogLine to just count calls.
				ProcessLogLine: func(line string, lineNumber int) {
					linesProcessed++
				},
				// Capture log output for assertions.
				LogFunc: func(level LogLevel, tag string, format string, args ...interface{}) {
					logMutex.Lock()
					capturedLogs = append(capturedLogs, fmt.Sprintf(format, args...))
					logMutex.Unlock()
				},
			}

			done := make(chan struct{})

			// --- Act ---
			DryRunLogProcessor(processor, done)
			<-done // Wait for the processor to finish.

			// --- Assert ---
			if linesProcessed != tt.expectedLinesProcessed {
				t.Errorf("Expected %d lines to be processed, but got %d", tt.expectedLinesProcessed, linesProcessed)
			}

			logOutput := strings.Join(capturedLogs, "\n")
			if !strings.Contains(logOutput, tt.expectedLogContains) {
				t.Errorf("Expected log output to contain '%s', but it did not.\nFull Log:\n%s", tt.expectedLogContains, logOutput)
			}
		})
	}
}
