package main

import (
	"bufio"
	"errors"
	"io"
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
func DryRunLogProcessor(p *Processor, done chan<- struct{}) {
	p.LogFunc(LevelInfo, "DRYRUN", "MODE: Reading test logs from %s...", TestLogPath)

	file, err := os.Open(TestLogPath)
	if err != nil {
		p.LogFunc(LevelCritical, "FATAL", "Failed to open test log file %s: %v", TestLogPath, err)
		// In a dry-run, a fatal error means we're done.
		close(done)
		return
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineNumber := 0
	lineLimit := MaxLogLineSize

	for {
		lineNumber++
		line, readErr := ReadLineWithLimit(reader, lineLimit)

		if errors.Is(readErr, io.EOF) {
			break
		}
		if errors.Is(readErr, ErrLineSkipped) {
			p.LogFunc(LevelWarning, "DRYRUN_SKIP", "Line %d: Skipped (Length exceeded %d bytes).", lineNumber, lineLimit)
			continue
		}
		if readErr != nil {
			p.LogFunc(LevelError, "DRYRUN_ERROR", "Line %d: Read error: %v", lineNumber, readErr)
			continue
		}

		// 3. Process the line
		p.ProcessLogLine(line, lineNumber)
	}

	p.LogFunc(LevelInfo, "DRYRUN", "DryRun complete. Processed %d lines.", lineNumber)
	close(done)
}

// LiveLogTailer continuously tails a log file, handling rotation and truncation.
func LiveLogTailer(p *Processor) {
	// Signal handling for graceful shutdown
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

	// Inner loop for re-opening the file
	for {
		p.LogFunc(LevelInfo, "TAIL", "Starting log tailer on %s...", LogFilePath)

		file, err := os.Open(LogFilePath)
		if err != nil {
			// File not found on first attempt, wait and retry.
			p.LogFunc(LevelError, "TAIL_ERROR", "Failed to open log file %s: %v. Retrying in %v.", LogFilePath, err, ErrorRetryDelay)
			select {
			case <-time.After(ErrorRetryDelay):
				continue
			case s := <-signalCh:
				p.LogFunc(LevelInfo, "SHUTDOWN", "Received signal %v. Shutting down gracefully.", s)
				file.Close()
				return
			}
		}

		// Get initial file stats for rotation/truncation detection
		initialStat, statErr := file.Stat()
		var initialSize int64
		var initialDev, initialIno uint64
		if statErr == nil {
			initialSize = initialStat.Size()
			initialSysStat := initialStat.Sys().(*syscall.Stat_t)
			initialDev = initialSysStat.Dev
			initialIno = initialSysStat.Ino
			p.LogFunc(LevelDebug, "TAIL", "Initial file state: Size=%d, Inode=%d, Device=%d", initialSize, initialIno, initialDev)
		} else {
			p.LogFunc(LevelWarning, "TAIL", "Failed to get initial file stat: %v. Rotation detection disabled.", statErr)
		}

		// Seek to the end of the file if not the first run.
		// Assuming for a persistent tailer, we start at the end if the file already exists.
		file.Seek(0, io.SeekEnd)
		reader := bufio.NewReader(file)

		lineNumber := 0
		lineLimit := MaxLogLineSize
		isPollingForStatCheck := false

		// Local function to restart the outer loop after a delay
		restartTailing := func(delay time.Duration) {
			file.Close() // Close the current file handle
			if delay > 0 {
				select {
				case <-time.After(delay):
					// continue outer loop
				case s := <-signalCh:
					p.LogFunc(LevelInfo, "SHUTDOWN", "Received signal %v. Shutting down gracefully.", s)
					return // exit function
				}
			}
		}

		// Inner loop for reading new lines
		for {
			// Check for signals first
			select {
			case s := <-signalCh:
				p.LogFunc(LevelInfo, "SHUTDOWN", "Received signal %v. Shutting down gracefully.", s)
				file.Close()
				return
			default:
				// Continue reading
			}

			// 1. Read the line
			line, finalErr := ReadLineWithLimit(reader, lineLimit)
			lineNumber++

			// 2. Handle read errors (EOF or other)
			if finalErr != nil {
				if errors.Is(finalErr, ErrLineSkipped) {
					p.LogFunc(LevelWarning, "TAIL_SKIP", "Line %d: Skipped (Length exceeded %d bytes).", lineNumber, lineLimit)
					continue
				}

				if finalErr == io.EOF {
					// End of file reached, begin polling for new data or file changes
					isPollingForStatCheck = false // Reset stat check flag
					if statErr == nil {
						// Only check for rotation/truncation if initial stat succeeded
						currentStat, checkStatErr := os.Stat(LogFilePath)
						if checkStatErr == nil {
							// Check for truncation (size decreased)
							if currentStat.Size() < initialSize {
								p.LogFunc(LevelInfo, "TAIL", "Detected log file size reduction (truncation/rotation). Reopening file.")
								restartTailing(0) // Restart immediately
								break
							}

							// Check for Inode/Device change (rotation)
							currentSysStat := currentStat.Sys().(*syscall.Stat_t)
							if currentSysStat.Dev != initialDev || currentSysStat.Ino != initialIno {
								p.LogFunc(LevelInfo, "TAIL", "Detected log file rotation (Inode changed from %d to %d). Reopening file.", initialIno, currentSysStat.Ino)
								restartTailing(0) // Restart immediately
								break
							}
						} else {
							// Suppress repeated "Failed to stat" messages
							if !isPollingForStatCheck {
								p.LogFunc(LevelError, "TAIL_ERROR", "Failed to stat log path during EOF check: %v. Retrying every %v.", checkStatErr, ErrorRetryDelay)
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
					p.LogFunc(LevelError, "TAIL_ERROR", "Read error while tailing log file: %v. Reopening in %v.", finalErr, ErrorRetryDelay)
					restartTailing(ErrorRetryDelay) // Wait a bit on read error
					break                           // Break inner loop to trigger file re-opening
				}
			}

			// 3. Process the line
			p.ProcessLogLine(line, lineNumber)
		}
	}
}
