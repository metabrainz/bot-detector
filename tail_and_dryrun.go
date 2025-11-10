package main

import (
	"bot-detector/internal/logging"
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync/atomic"
	"syscall"
	"time"
)

// fileOpener defines the function signature for opening a file, returning our interface.
type fileOpener func(name string) (fileHandle, error)

var osOpenFile fileOpener = func(name string) (fileHandle, error) {
	return os.Open(name)
}

// fileHandle defines the interface for file operations needed by the tailer.
type fileHandle interface {
	io.ReadSeeker
	io.Closer
	Stat() (os.FileInfo, error)
}

// lineReader is a function type for reading lines.
type lineReader func(reader *bufio.Reader, limit int) (string, error)

// handleLineRead is a common helper to process the result of a bufio.Reader.ReadBytes call.
func handleLineRead(line []byte, err error, limit int) (string, error) {
	if len(line) > limit {
		return string(line[:limit]), ErrLineSkipped
	}

	if err != nil {
		if err == io.EOF && len(line) > 0 {
			return string(line), io.EOF
		}
		return string(line), err
	}
	return string(line), nil

}

// readLineLF reads a line ending with LF ('\n').
func readLineLF(reader *bufio.Reader, limit int) (string, error) {
	line, err := reader.ReadBytes('\n')
	lineLen := len(line)
	if lineLen > 0 && line[lineLen-1] == '\n' {
		line = line[:lineLen-1] // Strip newline
	}
	return handleLineRead(line, err, limit)
}

// readLineCRLF reads a line ending with CRLF ('\r\n').
func readLineCRLF(reader *bufio.Reader, limit int) (string, error) {
	line, err := reader.ReadBytes('\n')
	lineLen := len(line)
	if lineLen > 1 && line[lineLen-2] == '\r' && line[lineLen-1] == '\n' {
		line = line[:lineLen-2] // Strip CRLF
	} else if lineLen > 0 && line[lineLen-1] == '\n' {
		line = line[:lineLen-1] // Fallback for just LF
	}
	return handleLineRead(line, err, limit)
}

// readLineCR reads a line ending with CR ('\r').
func readLineCR(reader *bufio.Reader, limit int) (string, error) {
	line, err := reader.ReadBytes('\r')
	lineLen := len(line)
	if lineLen > 0 && line[lineLen-1] == '\r' {
		line = line[:lineLen-1] // Strip carriage return
	}
	return handleLineRead(line, err, limit)
}

// getLineReader returns the appropriate line reading function based on the config.
func getLineReader(lineEnding string) (lineReader, error) {
	switch lineEnding {
	case "lf", "": // Default to 'lf' if empty
		return readLineLF, nil
	case "crlf":
		return readLineCRLF, nil
	case "cr":
		return readLineCR, nil
	default:
		return nil, fmt.Errorf("unsupported line ending: %s", lineEnding)
	}
}

// hasFileBeenRotated checks if the log file has been rotated or truncated.
// It returns true if the file should be reopened, false otherwise.
func hasFileBeenRotated(p *Processor, filePath string, initialStat os.FileInfo, statFunc func(string) (os.FileInfo, error)) bool {
	if initialStat == nil {
		// If we couldn't get initial stats, we can't detect rotation.
		return false
	}

	currentStat, err := statFunc(filePath)
	if err != nil {
		p.LogFunc(logging.LevelError, "TAIL_ERROR", "Failed to stat log path during EOF check: %v. Assuming rotation.", err)
		return true // If we can't stat the file, it might be gone. Reopen.
	}

	// Check for truncation (size decreased).
	if currentStat.Size() < initialStat.Size() {
		p.LogFunc(logging.LevelInfo, "TAIL", "Detected log file size reduction (truncation/rotation). Reopening file.")
		return true
	}

	// Check for Inode/Device change (rotation).
	initialSysStat := initialStat.Sys().(*syscall.Stat_t)
	currentSysStat := currentStat.Sys().(*syscall.Stat_t)
	if currentSysStat.Dev != initialSysStat.Dev || currentSysStat.Ino != initialSysStat.Ino {
		p.LogFunc(logging.LevelInfo, "TAIL", "Detected log file rotation (Inode changed from %d to %d). Reopening file.", initialSysStat.Ino, currentSysStat.Ino)
		return true
	}

	return false
}

func defaultStatFunc(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// delayOrShutdown waits for a specified duration but will return early if a shutdown
// signal is received on the provided channel. It returns true if a shutdown was triggered.
func delayOrShutdown(p *Processor, delay time.Duration, signalCh <-chan os.Signal) bool {
	select {
	case <-time.After(delay):
		return false // Delay completed
	case s := <-signalCh:
		p.LogFunc(logging.LevelInfo, "SHUTDOWN", "Received signal %v. Shutting down gracefully.", s)
		return true // Shutdown signal received
	}
}

// processFileLines is a shared helper function that reads a file line by line,
// handling different line endings and line length limits, and calls a processor function for each line.
func processFileLines(p *Processor, file io.Reader, lineProcessor func(line string)) error {
	// Select the line reader function based on config.
	readLine, err := getLineReader(p.Config.LineEnding)
	if err != nil {
		return fmt.Errorf("configuration error with line_ending: %w", err)
	}

	lineLimit := MaxLogLineSize

	reader := bufio.NewReader(file)
	for {
		line, readErr := readLine(reader, lineLimit)

		if readErr != nil {
			if errors.Is(readErr, ErrLineSkipped) {
				p.LogFunc(logging.LevelWarning, "TAIL_SKIP", "Skipped line (length exceeded %d bytes): %.100s...", lineLimit, line)
				continue
			}
			if readErr == io.EOF {
				// If we read a line and got EOF, process it before exiting.
				if len(line) > 0 {
					lineProcessor(line)
				}
				break // End of file
			}
			// For other read errors, log and break. The caller (LiveLogTailer) will handle reopening.
			p.LogFunc(logging.LevelError, "READ_ERROR", "Read error: %v", readErr)
			return readErr // Propagate the error up to the caller.
		}

		lineProcessor(line)
	}
	return nil
}

// DryRunLogProcessor reads and processes a static log file for testing.
func DryRunLogProcessor(p *Processor, done chan<- struct{}) {
	defer close(done)

	p.LogFunc(logging.LevelInfo, "DRY_RUN", "Starting dry-run mode for log file: %s", p.LogPath)
	startTime := time.Now()
	lineCount := 0

	file, err := osOpenFile(p.LogPath)
	if err != nil {
		p.LogFunc(logging.LevelCritical, "FATAL", "Failed to open log file %s: %v", p.LogPath, err)
		return
	}
	defer file.Close()

	// Use the shared line processing logic.
	err = processFileLines(p, file, func(line string) {
		p.ProcessLogLine(line)
		lineCount++
	})
	if err != nil {
		// Log the error if processing fails unexpectedly (e.g., config error).
		p.LogFunc(logging.LevelError, "DRY_RUN_ERROR", "Error during file processing: %v", err)
	}

	// After processing all lines, flush any remaining entries in the buffer.
	FlushEntryBuffer(p)
	elapsedTime := time.Since(startTime)

	// --- Metrics Calculation ---
	type chainMetric struct {
		Name  string
		Count int64
	}
	var completedMetrics []chainMetric
	var totalChainsCompleted int64

	var totalChainsReset int64

	p.Metrics.ChainsReset.Range(func(key, value interface{}) bool {
		if counter, ok := value.(*atomic.Int64); ok {
			totalChainsReset += counter.Load()
		}
		return true
	})
	p.Metrics.ChainsCompleted.Range(func(key, value interface{}) bool {
		chainName, ok1 := key.(string)
		counter, ok2 := value.(*atomic.Int64)
		if ok1 && ok2 {
			count := counter.Load()
			if count > 0 {
				completedMetrics = append(completedMetrics, chainMetric{Name: chainName, Count: count})
				totalChainsCompleted += count
			}
		}
		return true
	})

	// Sort metrics by count descending for a "top chains" view.
	sort.Slice(completedMetrics, func(i, j int) bool {
		return completedMetrics[i].Count > completedMetrics[j].Count
	})

	parseErrors := p.Metrics.ParseErrors.Load()
	reorderedEntries := p.Metrics.ReorderedEntries.Load()

	// --- Log Summary ---
	p.LogFunc(logging.LevelInfo, "DRY_RUN", "Dry-run finished.")
	p.LogFunc(logging.LevelInfo, "METRICS", "Lines Processed: %d", lineCount)
	p.LogFunc(logging.LevelInfo, "METRICS", "Chains Completed: %d", totalChainsCompleted)
	p.LogFunc(logging.LevelInfo, "METRICS", "Chains Reset: %d", totalChainsReset)
	p.LogFunc(logging.LevelInfo, "METRICS", "Parse Errors: %d", parseErrors)
	p.LogFunc(logging.LevelInfo, "METRICS", "Reordered Entries: %d", reorderedEntries)
	p.LogFunc(logging.LevelInfo, "METRICS", "Time Elapsed: %v", elapsedTime)

	if elapsedTime.Seconds() > 0 {
		linesPerSecond := float64(lineCount) / elapsedTime.Seconds()
		p.LogFunc(logging.LevelInfo, "METRICS", "Rate: %.2f lines/sec", linesPerSecond)
	} else {
		p.LogFunc(logging.LevelInfo, "METRICS", "Rate: n/a (run too fast)")
	}

	// Log the per-chain breakdown if any chains were completed.
	if len(completedMetrics) > 0 {
		p.LogFunc(logging.LevelInfo, "METRICS", "--- Chain Completion Breakdown ---")
		for _, metric := range completedMetrics {
			p.LogFunc(logging.LevelInfo, "METRICS", "  - %s: %d", metric.Name, metric.Count)
		}
	}
}

// LiveLogTailer continuously tails a log file, handling rotation and truncation.
func LiveLogTailer(p *Processor, signalCh <-chan os.Signal, readySignal chan<- struct{}) {
	var (
		firstRun = true // Flag to control initial seek behavior.
		shutdown = false
	)

	// Inner loop for re-opening the file
	for {
		var file fileHandle
		if shutdown {
			return
		}

		// Local function to restart the outer loop after a delay.
		// It's defined here to capture 'shutdown' in its closure.
		restartTailing := func(delay time.Duration) {
			if delay > 0 && delayOrShutdown(p, delay, signalCh) {
				shutdown = true
			}
		}

		p.LogFunc(logging.LevelInfo, "TAIL", "Starting log tailer on %s...", p.LogPath)

		file, err := osOpenFile(p.LogPath)
		if err != nil {
			// File not found on first attempt, wait and retry.
			p.LogFunc(logging.LevelError, "TAIL_ERROR", "Failed to open log file %s: %v. Retrying in %v.", p.LogPath, err, ErrorRetryDelay)
			if delayOrShutdown(p, ErrorRetryDelay, signalCh) {
				shutdown = true
				continue // Let the main loop handle the exit.
			}
			continue
		}

		// Get initial file stats for rotation/truncation detection
		initialStat, statErr := file.Stat()
		if statErr == nil {
			initialSysStat := initialStat.Sys().(*syscall.Stat_t)
			p.LogFunc(logging.LevelDebug, "TAIL", "Initial file state: Size=%d, Inode=%d, Device=%d", initialStat.Size(), initialSysStat.Ino, initialSysStat.Dev)
		} else {
			p.LogFunc(logging.LevelWarning, "TAIL_WARN", "Failed to get initial file stat: %v. Rotation detection may be impaired.", statErr)
			// If we can't stat the file, the handle is likely bad. Close it and restart the loop.
			file.Close()
			restartTailing(ErrorRetryDelay) // Add a delay to prevent a tight loop on repeated stat failures.
			if shutdown {
				continue // Let the main loop handle the exit, consistent with other error paths.
			}
			continue
		}

		// On the very first run, seek to the end to ignore old content.
		// On subsequent runs (after rotation), we read the new file from the beginning.
		if firstRun {
			file.Seek(0, io.SeekEnd)
			firstRun = false
		}

		// Signal for test synchronization, if the channel is set.
		if readySignal != nil {
			readySignal <- struct{}{}
		}

		reader := bufio.NewReader(file)
		readLine, err := getLineReader(p.Config.LineEnding)
		if err != nil {
			p.LogFunc(logging.LevelError, "TAIL_ERROR", "Configuration error with line_ending: %v. Retrying.", err)
			file.Close()
			restartTailing(ErrorRetryDelay)
			continue
		}
		lineLimit := MaxLogLineSize

		// Inner loop for reading new lines. This loop will be broken by file rotation or shutdown.
		for {
			select {
			case s := <-signalCh:
				p.LogFunc(logging.LevelInfo, "SHUTDOWN", "Received signal %v. Shutting down gracefully.", s)
				FlushEntryBuffer(p) // Final flush on shutdown.
				file.Close()
				return
			default:
				// Continue to read.
			}

			line, readErr := readLine(reader, lineLimit)

			if readErr != nil {
				if errors.Is(readErr, ErrLineSkipped) {
					p.LogFunc(logging.LevelWarning, "TAIL_SKIP", "Skipped line (length exceeded %d bytes): %.100s...", lineLimit, line)
					continue
				}
				if readErr == io.EOF {
					FlushEntryBuffer(p)
					if hasFileBeenRotated(p, p.LogPath, initialStat, p.Config.StatFunc) {
						file.Close()
						restartTailing(FileOpenRetryDelay) // Add delay to prevent tight loop on stat errors.
						break                              // Break inner loop to reopen.
					}
					time.Sleep(p.Config.EOFPollingDelay)
					continue
				}
				p.LogFunc(logging.LevelError, "TAIL_ERROR", "Read error while tailing log file: %v. Reopening file.", readErr)
				file.Close()
				restartTailing(ErrorRetryDelay)
				break // Break the inner loop to force a file reopen via the outer loop.
			}

			p.ProcessLogLine(line)
		}
	}
}
