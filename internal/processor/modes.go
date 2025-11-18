package processor

import (
	"bot-detector/internal/app"
	"bot-detector/internal/checker"
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/store"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"syscall"
	"time"
)

var (
	// ErrFileRotated is a sentinel error used to signal that the tailed file
	// has been rotated or truncated and should be reopened.
	ErrFileRotated = errors.New("file has been rotated")

	// ErrEOF is a sentinel error used to indicate that the end of the file
	// has been reached, but no rotation was detected. The caller should retry.
	ErrEOF = errors.New("end of file reached without rotation")
)

// Tailer is a struct that encapsulates the state and logic for tailing a single file.
type Tailer struct {
	path        string
	file        config.FileHandle
	reader      *bufio.Reader
	initialStat os.FileInfo
	logger      func(logging.LogLevel, string, string, ...interface{})
	config      struct {
		EOFPollingDelay time.Duration
		LineEnding      string
		FileOpener      func(string) (config.FileHandle, error)
		StatFunc        func(string) (os.FileInfo, error)
	}
}

// NewTailer creates and initializes a new Tailer. It opens the file, gets its
// initial stats for rotation detection, and seeks to the end for live tailing.
func NewTailer(p *app.Processor, seekToEnd bool) (*Tailer, error) {
	t := &Tailer{
		path:   p.LogPath,
		logger: p.LogFunc,
	}
	t.config.EOFPollingDelay = p.Config.Application.EOFPollingDelay
	t.config.LineEnding = p.Config.Parser.LineEnding
	t.config.FileOpener = p.Config.FileOpener
	t.config.StatFunc = p.Config.StatFunc

	var err error
	t.file, err = t.config.FileOpener(t.path)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file %s: %w", t.path, err)
	}

	t.initialStat, err = t.file.Stat()
	if err != nil {
		_ = t.file.Close()
		return nil, fmt.Errorf("failed to get initial file stat for %s: %w", t.path, err)
	}
	t.logger(logging.LevelDebug, "TAIL", "Initial file state: Size=%d", t.initialStat.Size())

	if seekToEnd {
		if _, err := t.file.Seek(0, io.SeekEnd); err != nil {
			_ = t.file.Close()
			return nil, fmt.Errorf("failed to seek to end of file %s: %w", t.path, err)
		}
	}

	t.reader = bufio.NewReader(t.file)
	return t, nil
}

// ReadLine reads a single line from the tailed file. It handles EOF by checking
// for file rotation and returns sentinel errors to signal the outcome.
func (t *Tailer) ReadLine() (string, error) {
	readLine, err := getLineReader(t.config.LineEnding)
	if err != nil {
		return "", fmt.Errorf("configuration error with line_ending: %w", err)
	}
	lineLimit := config.MaxLogLineSize

	line, readErr := readLine(t.reader, lineLimit)

	if readErr != nil {
		if errors.Is(readErr, config.ErrLineSkipped) {
			t.logger(logging.LevelWarning, "TAIL_SKIP", "Skipped line (length exceeded %d bytes): %.100s...", lineLimit, line)
			return "", config.ErrLineSkipped // Return the sentinel error
		}
		if readErr == io.EOF {
			// If we read a line along with EOF (file without trailing newline),
			// return the line without error. The next call will handle the EOF.
			if len(line) > 0 {
				return line, nil
			}
			if t.checkForRotation() {
				return "", ErrFileRotated
			}
			// Return ErrEOF immediately without sleeping.
			// The caller (LiveLogTailer) will handle the sleep, allowing
			// it to check for shutdown signals during the delay.
			return "", ErrEOF
		}
		return "", readErr // Return other unexpected errors
	}

	return line, nil
}

// Close closes the underlying file handle.
func (t *Tailer) Close() error {
	if t.file != nil {
		return t.file.Close()
	}
	return nil
}

// checkForRotation checks if the log file has been rotated or truncated.
// It returns true if the file should be reopened, false otherwise.
func (t *Tailer) checkForRotation() bool {
	if t.initialStat == nil {
		return false
	}

	currentPathStat, err := t.config.StatFunc(t.path)
	if err != nil {
		t.logger(logging.LevelError, "TAIL_ERROR", "Failed to stat log path during EOF check: %v. Assuming rotation.", err)
		return true
	}

	// Check for truncation: if the file at the path is smaller than when we opened it.
	// This detects copytruncate-style rotation.
	if currentPathStat.Size() < t.initialStat.Size() {
		t.logger(logging.LevelInfo, "TAIL", "Detected log file size reduction (truncation/rotation). Reopening file.")
		return true
	}

	// Check for rotation by comparing the open file handle's stat with the path's stat.
	// After rotation (rename + create), the path points to a different file than our handle.
	// This works even when Sys() returns nil (no inode support).
	if t.file != nil {
		currentFileStat, err := t.file.Stat()
		if err != nil {
			t.logger(logging.LevelError, "TAIL_ERROR", "Failed to stat open file handle: %v. Assuming rotation.", err)
			return true
		}

		// If the path's file differs from our open file, rotation occurred.
		// We check size and modification time as they're available without inode support.
		if currentFileStat.Size() != currentPathStat.Size() {
			t.logger(logging.LevelInfo, "TAIL", "Detected log file rotation (size mismatch: open file=%d, path file=%d). Reopening file.", currentFileStat.Size(), currentPathStat.Size())
			return true
		}
	}

	// Additional check: inode-based detection if available (most reliable method).
	initialSys := t.initialStat.Sys()
	currentSys := currentPathStat.Sys()

	if initialSys != nil && currentSys != nil {
		initialSysStat, ok1 := initialSys.(*syscall.Stat_t)
		currentSysStat, ok2 := currentSys.(*syscall.Stat_t)

		if ok1 && ok2 {
			if currentSysStat.Dev != initialSysStat.Dev || currentSysStat.Ino != initialSysStat.Ino {
				t.logger(logging.LevelInfo, "TAIL", "Detected log file rotation (Inode changed from %d to %d). Reopening file.", initialSysStat.Ino, currentSysStat.Ino)
				return true
			}
		} else {
			t.logger(logging.LevelWarning, "TAIL_WARN", "Could not assert syscall.Stat_t for initial or current file. Inode/Device rotation detection impaired.")
		}
	} else {
		t.logger(logging.LevelDebug, "TAIL_DEBUG", "Sys() call returned nil for initial or current file. Inode/Device rotation detection skipped.")
	}

	return false
}

// LineReader is a function type for reading lines.
type LineReader func(reader *bufio.Reader, limit int) (string, error)

// handleLineRead is a common helper to process the result of a bufio.Reader.ReadBytes call.
func handleLineRead(line []byte, err error, limit int) (string, error) {
	if len(line) > limit {
		return string(line[:limit]), config.ErrLineSkipped
	}

	if err != nil {
		if err == io.EOF && len(line) > 0 {
			return string(line), io.EOF
		}
		return string(line), err
	}
	return string(line), nil

}

// ReadLineLF reads a line ending with LF ('\n').
func ReadLineLF(reader *bufio.Reader, limit int) (string, error) {
	line, err := reader.ReadBytes('\n')
	lineLen := len(line)
	if lineLen > 0 && line[lineLen-1] == '\n' {
		line = line[:lineLen-1] // Strip newline
	}
	return handleLineRead(line, err, limit)
}

// ReadLineCRLF reads a line ending with CRLF ('\r\n').
func ReadLineCRLF(reader *bufio.Reader, limit int) (string, error) {
	line, err := reader.ReadBytes('\n')
	lineLen := len(line)
	if lineLen > 1 && line[lineLen-2] == '\r' && line[lineLen-1] == '\n' {
		line = line[:lineLen-2] // Strip CRLF
	} else if lineLen > 0 && line[lineLen-1] == '\n' {
		line = line[:lineLen-1] // Fallback for just LF
	}
	return handleLineRead(line, err, limit)
}

// ReadLineCR reads a line ending with CR ('\r').
func ReadLineCR(reader *bufio.Reader, limit int) (string, error) {
	line, err := reader.ReadBytes('\r')
	lineLen := len(line)
	if lineLen > 0 && line[lineLen-1] == '\r' {
		line = line[:lineLen-1] // Strip carriage return
	}
	return handleLineRead(line, err, limit)
}

// getLineReader returns the appropriate line reading function based on the config.
func getLineReader(lineEnding string) (LineReader, error) {
	switch lineEnding {
	case "lf", "": // Default to 'lf' if empty
		return ReadLineLF, nil
	case "crlf":
		return ReadLineCRLF, nil
	case "cr":
		return ReadLineCR, nil
	default:
		return nil, fmt.Errorf("unsupported line ending: %s", lineEnding)
	}
}

// DefaultStatFunc is the default file stat function using os.Stat.
// Exported for use in tests.
func DefaultStatFunc(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// delayOrShutdown waits for a specified duration but will return early if a shutdown
// signal is received on the provided channel. It returns true if a shutdown was triggered.
func DelayOrShutdown(p *app.Processor, delay time.Duration, signalCh <-chan os.Signal) bool {
	select {
	case <-time.After(delay):
		return false // Delay completed
	case s := <-signalCh:
		p.LogFunc(logging.LevelInfo, "SHUTDOWN", "Received signal %v. Shutting down gracefully.", s)
		// Re-broadcast the signal so other listeners (like in main()) can also receive it.
		// This is crucial for a coordinated shutdown.
		if p.SignalCh != nil {
			p.SignalCh <- s
		}
		return true // Shutdown signal received
	}
}

// processFileLines is a shared helper function that reads a file line by line,
// handling different line endings and line length limits, and calls a processor function for each line.
func processFileLines(p *app.Processor, file io.Reader, lineProcessor func(line string)) error {
	// Select the line reader function based on config.
	readLine, err := getLineReader(p.Config.Parser.LineEnding)
	if err != nil {
		return fmt.Errorf("configuration error with line_ending: %w", err)
	}

	lineLimit := config.MaxLogLineSize

	reader := bufio.NewReader(file)
	for {
		line, readErr := readLine(reader, lineLimit)

		if readErr != nil {
			if errors.Is(readErr, config.ErrLineSkipped) {
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
func DryRunLogProcessor(p *app.Processor, done chan<- struct{}) {
	defer close(done)

	var reader io.Reader
	var logSource string

	if p.LogPath != "" {
		logSource = fmt.Sprintf("log file: %s", p.LogPath)
		file, err := p.Config.FileOpener(p.LogPath)
		if err != nil {
			p.LogFunc(logging.LevelCritical, "FATAL", "Failed to open log file %s: %v", p.LogPath, err)
			return
		}
		defer func() {
			_ = file.Close()
		}()

		// --- Magic Number Detection for file-based input ---
		magicBuf := make([]byte, 3)
		bytesRead, err := file.Read(magicBuf)
		if err != nil && err != io.EOF {
			p.LogFunc(logging.LevelCritical, "FATAL", "Failed to read from log file %s for magic number detection: %v", p.LogPath, err)
			return
		}

		if _, err := file.Seek(0, io.SeekStart); err != nil {
			p.LogFunc(logging.LevelCritical, "FATAL", "Failed to seek to start of log file %s: %v", p.LogPath, err)
			return
		}

		reader = file
		if bytesRead >= 2 && bytes.Equal(magicBuf[:2], []byte{0x1f, 0x8b}) {
			gzReader, err := gzip.NewReader(file)
			if err != nil {
				p.LogFunc(logging.LevelCritical, "FATAL", "Failed to create gzip reader for %s: %v", p.LogPath, err)
				return
			}
			defer func() {
				_ = gzReader.Close()
			}()
			reader = gzReader
			p.LogFunc(logging.LevelInfo, "DRY_RUN", "Detected gzip format. Decompressing log file on the fly...")
		} else if bytesRead >= 3 && bytes.Equal(magicBuf[:3], []byte{'B', 'Z', 'h'}) {
			reader = bzip2.NewReader(file)
			p.LogFunc(logging.LevelInfo, "DRY_RUN", "Detected bzip2 format. Decompressing log file on the fly...")
		}
	} else {
		logSource = "stdin"
		reader = os.Stdin
	}

	p.LogFunc(logging.LevelInfo, "DRY_RUN", "Starting dry-run mode from %s", logSource)
	startTime := time.Now()
	p.TopActorsPerChain = make(map[string]map[string]*store.ActorStats) // Initialize for this dry run.

	// Use the shared line processing logic.
	err := processFileLines(p, reader, func(line string) {
		p.ProcessLogLine(line)
		p.Metrics.LinesProcessed.Add(1)
	})
	if err != nil {
		// Log the error if processing fails unexpectedly (e.g., config error).
		p.LogFunc(logging.LevelError, "DRY_RUN_ERROR", "Error during file processing: %v", err)
	}
	// After processing all lines, flush any remaining entries in the buffer.
	checker.FlushEntryBuffer(p)
	elapsedTime := time.Since(startTime)

	p.LogFunc(logging.LevelInfo, "DRY_RUN", "Dry-run finished.")
	app.LogMetricsSummary(p, elapsedTime, p.LogFunc, "METRICS", "dryrun")
}

// logTopActorsSummary displays the top N actors per chain if the feature is enabled.
func CleanupTopActors(p *app.Processor, stop <-chan struct{}) {
	if p.TopN <= 0 {
		p.LogFunc(logging.LevelDebug, "CLEANUP_TOPN", "Top-N cleanup routine is disabled (top-n <= 0).")
		return // Cleanup is disabled.
	}

	p.ConfigMutex.RLock()
	cleanupInterval := p.Config.Checker.ActorCleanupInterval
	p.ConfigMutex.RUnlock()

	if cleanupInterval == 0 {
		cleanupInterval = 1 * time.Minute // Default to 1 minute if not set.
	}

	p.LogFunc(logging.LevelDebug, "CLEANUP_TOPN", "Starting Top-N cleanup routine, running every %v.", cleanupInterval)
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			p.LogFunc(logging.LevelDebug, "CLEANUP_TOPN", "Stopping Top-N cleanup routine.")
			return
		case <-ticker.C:
			p.LogFunc(logging.LevelDebug, "CLEANUP_TOPN", "Running Top-N cleanup...")
			p.ActivityMutex.Lock()

			for chainName, actors := range p.TopActorsPerChain {
				if len(actors) <= p.TopN {
					continue // No need to clean up if we're under the limit.
				}

				// Convert map to slice for sorting.
				type actorStat struct {
					Actor string
					Stats *store.ActorStats
				}
				var statsList []actorStat
				for actor, stats := range actors {
					statsList = append(statsList, actorStat{Actor: actor, Stats: stats})
				}

				// Sort actors by activity (hits > completions > resets).
				sort.Slice(statsList, func(i, j int) bool {
					return statsList[i].Stats.IsMoreActiveThan(statsList[j].Stats)
				})

				// Create a new map with only the top N actors.
				newActors := make(map[string]*store.ActorStats)
				for i := 0; i < p.TopN && i < len(statsList); i++ {
					newActors[statsList[i].Actor] = statsList[i].Stats
				}
				p.TopActorsPerChain[chainName] = newActors
			}
			p.ActivityMutex.Unlock()
		}
	}
}

// LiveLogTailer continuously tails a log file, handling rotation and truncation.
func LiveLogTailer(p *app.Processor, signalCh <-chan os.Signal, readySignal chan<- struct{}) {
	var (
		firstRun = true // Flag to control initial seek behavior.
		shutdown = false
	)

	// Inner loop for re-opening the file
	for {
		if shutdown {
			return
		}

		// Local function to restart the outer loop after a delay.
		// It's defined here to capture 'shutdown' in its closure.
		restartTailing := func(delay time.Duration) {
			if delay > 0 && DelayOrShutdown(p, delay, signalCh) {
				shutdown = true
			}
		}

		p.LogFunc(logging.LevelInfo, "TAIL", "Starting log tailer on %s...", p.LogPath)

		// Determine whether to seek to end on this iteration.
		// On the very first run, seek to the end to ignore old content,
		// but only if we're not in exit-on-eof mode.
		seekToEnd := firstRun && !p.ExitOnEOF
		isFirstRun := firstRun // Save before modifying
		firstRun = false

		tailer, err := NewTailer(p, seekToEnd)
		if err != nil {
			// File not found or error on initial open, wait and retry.
			p.LogFunc(logging.LevelError, "TAIL_ERROR", "Failed to open log file %s: %v. Retrying in %v.", p.LogPath, err, config.ErrorRetryDelay)
			restartTailing(config.ErrorRetryDelay)
			continue
		}

		// Signal for test synchronization, if the channel is set.
		// IMPORTANT: Only signal on the FIRST successful open, not on reopens after rotation.
		// Signaling on every reopen can cause deadlock if nothing is listening.
		if readySignal != nil && isFirstRun {
			readySignal <- struct{}{}
		}

		// Inner loop for reading new lines. This loop will be broken by file rotation or shutdown.
		for {
			select {
			case s := <-signalCh:
				p.LogFunc(logging.LevelInfo, "SHUTDOWN", "Received signal %v. Shutting down gracefully.", s)
				checker.FlushEntryBuffer(p) // Final flush on shutdown.
				_ = tailer.Close()
				return
			default:
				// Continue to read.
			}

			line, readErr := tailer.ReadLine()

			if readErr != nil {
				if errors.Is(readErr, config.ErrLineSkipped) {
					// Already logged by tailer, just continue
					continue
				}
				if errors.Is(readErr, ErrEOF) {
					checker.FlushEntryBuffer(p)
					// If the flag is set, we exit on the first EOF.
					if p.ExitOnEOF {
						p.LogFunc(logging.LevelInfo, "TAIL", "Reached EOF, exiting due to --exit-on-eof flag.")
						_ = tailer.Close()
						return // Exit the function entirely
					}
					// ErrEOF means we hit EOF but no rotation was detected.
					// Sleep before polling again, allowing shutdown signals to be checked.
					time.Sleep(p.Config.Application.EOFPollingDelay)
					continue
				}
				if errors.Is(readErr, ErrFileRotated) {
					// File has been rotated, close current tailer and reopen
					_ = tailer.Close()
					restartTailing(config.FileOpenRetryDelay)
					break // Break inner loop to reopen.
				}
				// Other unexpected errors
				p.LogFunc(logging.LevelError, "TAIL_ERROR", "Read error while tailing log file: %v. Reopening file.", readErr)
				_ = tailer.Close()
				restartTailing(config.ErrorRetryDelay)
				break // Break the inner loop to force a file reopen via the outer loop.
			}

			p.ProcessLogLine(line)
			p.Metrics.LinesProcessed.Add(1)
		}
	}
}
