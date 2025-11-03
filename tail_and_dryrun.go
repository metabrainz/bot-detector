package main

import (
	"bufio"
	"errors"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ReadLineWithLimit reads a line from the reader up to the given limit (in bytes).
// If the line exceeds the limit, it returns the partial line and ErrLineSkipped.
func ReadLineWithLimit(reader *bufio.Reader, limit int) (string, error) {
	var line []byte
	var isPrefix bool = true
	var err error

	for len(line) < limit {
		var chunk []byte
		chunk, isPrefix, err = reader.ReadLine()
		line = append(line, chunk...)

		if err != nil {
			// io.EOF is the standard end-of-file signal
			return string(line), err
		}

		if !isPrefix {
			// Whole line read (line ends with '\n')
			return string(line), nil
		}

		// If isPrefix is true here, the line exceeded the buffer and possibly the limit.
		if len(line) >= limit {
			// Discard the remainder of the line from the buffer up to the next newline.
			_, _ = reader.ReadString('\n')
			return string(line), ErrLineSkipped
		}
	}

	return string(line), io.EOF
}

// DryRunLogProcessor reads and processes a static log file for testing.
func DryRunLogProcessor(done chan<- struct{}) {
	LogOutput(LevelInfo, "DRYRUN", "MODE: Reading test logs from %s...", TestLogPath)

	file, err := os.Open(TestLogPath)
	if err != nil {
		log.Fatalf("[FATAL] Dry Run Failed: Could not open test log file %s: %v", TestLogPath, err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineNumber := 0

	for {
		line, err := ReadLineWithLimit(reader, MaxLogLineSize)

		if err != nil {
			if errors.Is(err, io.EOF) {
				// Process final line fragment if present
				if line != "" {
					lineNumber++
					ProcessLogLine(line, lineNumber)
				}
				break
			}
			if errors.Is(err, ErrLineSkipped) {
				LogOutput(LevelWarning, "SKIPPED", "Line exceeded critical limit and was skipped.")
				continue
			}

			LogOutput(LevelError, "DRYRUN_ERROR", "Reading log file: %v. Exiting dry-run loop.", err)
			break
		}

		lineNumber++
		ProcessLogLine(line, lineNumber)
	}

	LogOutput(LevelInfo, "DRYRUN", "Log file processing complete. Total lines: %d", lineNumber)
	done <- struct{}{}
}

// LiveLogTailer is the main loop for reading a log file that is being actively written to.
func LiveLogTailer() {
	LogOutput(LevelInfo, "LIVE", "MODE: Starting live log tailer on %s...", LogFilePath)

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		LogOutput(LevelCritical, "SHUTDOWN", "Received signal %v. Initiating graceful shutdown.", sig)
		// Perform any cleanup here if necessary
		os.Exit(0)
	}()

	var initialStat os.FileInfo
	var initialDev, initialIno uint64
	var file *os.File
	var reader *bufio.Reader
	lineNumber := 0

	// Polling state flags to suppress repeated log messages
	var isPollingForFileOpen bool = false
	var isPollingForStatCheck bool = false

	// Delays
	const (
		FileOpenRetryDelay = 100 * time.Millisecond // For quick polling when the file is missing (e.g., just after rotation)
		EOFPollingDelay    = 200 * time.Millisecond // For regular polling when hitting EOF on an open file
		ErrorRetryDelay    = 1 * time.Second        // For persistent errors (read failures, stat failures)
	)

	// Main tailing loop (re-opens file on rotation/truncation)
	for {
		// Helper closure to close the file, reset variables, and optionally sleep before the next attempt.
		restartTailing := func(delay time.Duration) {
			if file != nil {
				file.Close()
				file = nil
			}
			if delay > 0 {
				time.Sleep(delay)
			}
		}

		// 1. Open the file
		if file == nil {
			var err error
			file, err = os.Open(LogFilePath)
			if err != nil {
				// Suppress repeated "Failed to open" messages during fast polling
				if !isPollingForFileOpen {
					LogOutput(LevelError, "TAIL_ERROR", "Failed to open log file %s: %v. Retrying every %v.", LogFilePath, err, FileOpenRetryDelay)
					isPollingForFileOpen = true
				}

				// Wait for the quick poll interval silently
				time.Sleep(FileOpenRetryDelay)
				continue
			}

			// SUCCESSFUL OPEN: Check if we just recovered from a file-open failure or a stat-check failure
			if isPollingForFileOpen {
				LogOutput(LevelInfo, "TAIL_RECOVER", "Successfully reopened log file %s after polling.", LogFilePath)
				isPollingForFileOpen = false
			}

			// If we successfully opened the file, we are no longer in the stat polling error state.
			if isPollingForStatCheck {
				isPollingForStatCheck = false
				LogOutput(LevelInfo, "TAIL_RECOVER", "Stat check recovered implicitly by successful file open.")
			}

			reader = bufio.NewReader(file)

			// Get initial file stats from the opened file handle to prevent a race condition.
			newInitialStat, statErr := file.Stat()
			if statErr != nil {
				LogOutput(LevelWarning, "TAIL_ERROR", "Failed to stat opened file: %v. Proceeding without full rotation check.", statErr)
				initialStat = nil // Ensure initialStat is explicitly nil on failure
			} else {
				initialStat = newInitialStat
				// Guaranteed to be on Linux, assert to syscall.Stat_t
				initialSysStat := initialStat.Sys().(*syscall.Stat_t)
				initialDev = initialSysStat.Dev
				initialIno = initialSysStat.Ino

				// When opening, jump to the end for live tailing
				_, err = file.Seek(0, io.SeekEnd)
				if err != nil {
					LogOutput(LevelWarning, "TAIL_ERROR", "Failed to seek to end of file: %v", err)
				} else {
					LogOutput(LevelInfo, "TAIL", "Tailing started from end of file.")
				}
			}
		}

		// 2. Read lines in a sub-loop
		for {
			// This call will block until a new line is available or an error occurs (like EOF)
			line, err := ReadLineWithLimit(reader, MaxLogLineSize)
			finalErr := err

			if errors.Is(finalErr, ErrLineSkipped) {
				LogOutput(LevelWarning, "SKIPPED", "Live log line exceeded critical limit of %dKB and was skipped.", MaxLogLineSize/1024)
				continue
			}

			if finalErr != nil {
				if finalErr == io.EOF { // Standard check for live tail: sleep and check rotation
					// Only perform rotation checks if initialStat was successfully captured.
					if initialStat != nil {
						currentStat, statErr := os.Stat(LogFilePath)
						if statErr == nil {
							// SUCCESSFUL STAT CHECK: If we were polling, log recovery and reset flag
							if isPollingForStatCheck {
								LogOutput(LevelInfo, "TAIL_RECOVER", "Stat check successful, exiting stat error polling mode.")
								isPollingForStatCheck = false
							}

							// Check for truncation
							if currentStat.Size() < initialStat.Size() {
								LogOutput(LevelDebug, "TAIL", "Detected log file size reduction (truncation/rotation). Reopening file.")
								restartTailing(0) // Restart immediately
								break
							}

							// Check for Inode/Device change (rotation)
							currentSysStat := currentStat.Sys().(*syscall.Stat_t)
							if currentSysStat.Dev != initialDev || currentSysStat.Ino != initialIno {
								LogOutput(LevelInfo, "TAIL", "Detected log file rotation (Inode changed from %d to %d). Reopening file.", initialIno, currentSysStat.Ino)
								restartTailing(0) // Restart immediately
								break
							}
						} else {
							// Suppress repeated "Failed to stat" messages
							if !isPollingForStatCheck {
								LogOutput(LevelError, "TAIL_ERROR", "Failed to stat log path during EOF check: %v. Retrying every %v.", statErr, ErrorRetryDelay)
								isPollingForStatCheck = true
							}
							restartTailing(ErrorRetryDelay) // Wait a bit on stat failure
							break
						}
					}

					time.Sleep(EOFPollingDelay) // Use EOFPollingDelay for standard polling
					continue
				} else {
					// Read error (non-EOF) is typically a one-off event, but we retry
					LogOutput(LevelError, "TAIL_ERROR", "Read error while tailing log file: %v. Reopening in %v.", finalErr, ErrorRetryDelay)
					restartTailing(ErrorRetryDelay) // Wait a bit on read error
					break                           // Break inner loop to trigger file re-opening
				}
			}

			// 3. Process the log line
			lineNumber++
			ProcessLogLine(line, lineNumber)
		}
	}
}
