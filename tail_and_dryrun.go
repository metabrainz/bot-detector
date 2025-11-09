package main

import (
	"bot-detector/internal/logging"
	"bufio"
	"errors"
	"io"
	"os"
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

// ReadLineWithLimit reads a line from the reader up to the given limit (in bytes).
// If the line exceeds the limit, it returns the partial line and ErrLineSkipped.
// It correctly handles `\n`, `\r`, and `\r\n` line endings.
func ReadLineWithLimit(reader *bufio.Reader, limit int) (string, error) {
	var line []byte
	for {
		b, err := reader.ReadByte()
		if err != nil {
			// If we have content and hit EOF, it's a valid last line.
			if err == io.EOF && len(line) > 0 {
				if len(line) > limit {
					return string(line[:limit]), ErrLineSkipped
				}
				return string(line), io.EOF
			}
			// For any other error (including EOF on an empty read), return it.
			return string(line), err
		}

		if b == '\n' {
			// Unix EOL. We're done.
			break
		}

		if b == '\r' {
			// Could be Windows (\r\n) or classic Mac (\r).
			// Peek at the next byte to see if it's a '\n'.
			if nextByte, err := reader.Peek(1); err == nil && nextByte[0] == '\n' {
				reader.ReadByte() // It's '\r\n', so consume the '\n' as well.
			}
			break // In both cases (\r or \r\n), we're done with this line.
		}

		line = append(line, b)
	}

	if len(line) > limit {
		return string(line[:limit]), ErrLineSkipped
	}

	return string(line), nil
}

// DryRunLogProcessor reads and processes a static log file for testing.
func DryRunLogProcessor(p *Processor, done chan<- struct{}) {
	p.LogFunc(logging.LevelInfo, "DRYRUN", "MODE: Reading logs from %s...", LogFilePath)

	file, err := osOpenFile(LogFilePath)
	if err != nil {
		p.LogFunc(logging.LevelCritical, "FATAL", "Failed to open log file %s: %v", LogFilePath, err)
		// In a dry-run, a fatal error means we're done.
		close(done)
		return
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineNumber := 0
	processedCount := 0
	lineLimit := MaxLogLineSize

	for {
		line, readErr := ReadLineWithLimit(reader, lineLimit)
		lineNumber++ // Increment after the read to accurately report the line number of the error.

		// Use a switch for clearer error handling.
		switch {
		case errors.Is(readErr, io.EOF):
			// If we read a line and got EOF, process it and then exit the loop.
			if len(line) > 0 {
				p.ProcessLogLine(line, lineNumber)
				processedCount++
			}
			goto endLoop // Use goto to break out of the outer for loop.
		case errors.Is(readErr, ErrLineSkipped):
			p.LogFunc(logging.LevelWarning, "DRYRUN_SKIP", "Line %d: Skipped (Length exceeded %d bytes).", lineNumber, lineLimit)
			continue
		case readErr != nil:
			p.LogFunc(logging.LevelError, "DRYRUN_ERROR", "Line %d: Read error: %v", lineNumber, readErr)
			continue
		}

		// Skip comments and empty lines before processing.
		if len(line) == 0 || line[0] == '#' {
			p.LogFunc(logging.LevelDebug, "DRYRUN_SKIP", "Line %d: Skipped (Comment/Empty).", lineNumber)
			continue
		}

		// 3. Process the line
		p.ProcessLogLine(line, lineNumber)
		processedCount++
	}

endLoop:
	p.LogFunc(logging.LevelInfo, "DRYRUN", "DryRun complete. Processed %d lines.", processedCount)
	close(done)
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

		p.LogFunc(logging.LevelInfo, "TAIL", "Starting log tailer on %s...", LogFilePath)

		file, err := osOpenFile(LogFilePath)
		if err != nil {
			// File not found on first attempt, wait and retry.
			p.LogFunc(logging.LevelError, "TAIL_ERROR", "Failed to open log file %s: %v. Retrying in %v.", LogFilePath, err, ErrorRetryDelay)
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

		// On the very first run, seek to the end of the file to ignore old content.
		// On subsequent runs (after rotation), we want to read the new file from the beginning.
		if firstRun {
			file.Seek(0, io.SeekEnd)
			firstRun = false
		}
		reader := bufio.NewReader(file)

		// Signal for test synchronization, if the channel is set.
		// This is done AFTER the initial seek to ensure the tailer is truly ready.
		if readySignal != nil {
			readySignal <- struct{}{}
		}

		lineNumber := 0
		lineLimit := MaxLogLineSize

		// Inner loop for reading new lines
		for {
			// Check for signals first
			select {
			case s := <-signalCh:
				p.LogFunc(logging.LevelInfo, "SHUTDOWN", "Received signal %v. Shutting down gracefully.", s)
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
					p.LogFunc(logging.LevelWarning, "TAIL_SKIP", "Line %d: Skipped (Length exceeded %d bytes).", lineNumber, lineLimit)
					continue
				}

				if finalErr == io.EOF {
					// At EOF, check for file rotation before sleeping.
					// For testing: signal that we are about to perform the check.
					if p.Config.eofCheckSignal != nil {
						p.Config.eofCheckSignal <- struct{}{}
					}

					if hasFileBeenRotated(p, LogFilePath, initialStat, p.Config.StatFunc) {
						// If hasFileBeenRotated failed because of a stat error, it's safer to add a small delay
						// before retrying to avoid a tight loop if the file is genuinely gone.
						// The restartTailing function handles the delay and shutdown check.
						file.Close()
						restartTailing(FileOpenRetryDelay) // Use a small delay to prevent tight loops.
						break                              // Break inner loop to reopen
					}
					time.Sleep(p.Config.EOFPollingDelay) // Use configurable polling delay
					continue
				} else {
					// Read error (non-EOF) is typically a one-off event, but we retry
					p.LogFunc(logging.LevelError, "TAIL_ERROR", "Read error while tailing log file: %v. Reopening in %v.", finalErr, ErrorRetryDelay)
					file.Close()
					restartTailing(ErrorRetryDelay)
					break // Break inner loop to trigger file re-opening
				}
			}

			// 3. Process the line
			p.ProcessLogLine(line, lineNumber) // Use the function field
		}
	}
}
