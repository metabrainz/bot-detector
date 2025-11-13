package main

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/store"
	"bot-detector/internal/utils"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
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
		// Re-broadcast the signal so other listeners (like in main()) can also receive it.
		// This is crucial for a coordinated shutdown.
		if p.signalCh != nil {
			p.signalCh <- s
		}
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

	var reader io.Reader
	var logSource string

	if p.LogPath != "" {
		logSource = fmt.Sprintf("log file: %s", p.LogPath)
		file, err := osOpenFile(p.LogPath)
		if err != nil {
			p.LogFunc(logging.LevelCritical, "FATAL", "Failed to open log file %s: %v", p.LogPath, err)
			return
		}
		defer file.Close()

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
			defer gzReader.Close()
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
	FlushEntryBuffer(p)
	elapsedTime := time.Since(startTime)

	p.LogFunc(logging.LevelInfo, "DRY_RUN", "Dry-run finished.")
	logMetricsSummary(p, elapsedTime, p.LogFunc, "METRICS", "dryrun")
}

// logTopActorsSummary displays the top N actors per chain if the feature is enabled.
func logTopActorsSummary(p *Processor, logFunc func(logging.LogLevel, string, string, ...interface{})) {
	if p.TopN <= 0 {
		return // Top-N reporting is disabled.
	}

	logFunc(logging.LevelInfo, "STATS", "--- Top %d Actors per Chain ---", p.TopN)
	if len(p.TopActorsPerChain) == 0 {
		logFunc(logging.LevelInfo, "STATS", "  (No chain activity to report)")
		return
	}

	// Get chain names and sort them for consistent output order.
	var chainNames []string
	for chainName := range p.TopActorsPerChain {
		chainNames = append(chainNames, chainName)
	}
	sort.Strings(chainNames)

	for _, chainName := range chainNames {
		actorHits := p.TopActorsPerChain[chainName]
		if len(actorHits) == 0 {
			continue
		}

		type actorStat struct {
			Actor string
			Stats *store.ActorStats
		}

		var stats []actorStat
		for actor, actorStats := range actorHits {
			stats = append(stats, actorStat{Actor: actor, Stats: actorStats})
		}

		// Sort actors primarily by hits, then by completions, then by resets (all descending).
		// The IsMoreActiveThan method handles the primary sorting logic.
		// A final sort by the actor string is used as a tie-breaker for stable ordering.
		sort.Slice(stats, func(i, j int) bool {
			return stats[i].Stats.IsMoreActiveThan(stats[j].Stats)
		})

		logFunc(logging.LevelInfo, "STATS", "  Chain: %s", chainName)
		limit := p.TopN
		for i, stat := range stats {
			// Only show actors with at least one hit.
			if i >= limit || stat.Stats.Hits == 0 {
				break
			}
			logFunc(logging.LevelInfo, "STATS", "    - [%d hits, %d completions, %d resets] %s",
				stat.Stats.Hits, stat.Stats.Completions, stat.Stats.Resets, stat.Actor)
		}
	}
}

// metricItem is a generic struct to hold data collected from a sync.Map.
type metricItem struct {
	Key   string
	Count int64
}

// collectMetricsFromMap is a helper to gather and sort metrics from a sync.Map.
func collectMetricsFromMap(m *sync.Map) []metricItem {
	var items []metricItem
	m.Range(func(key, value interface{}) bool {
		if k, ok := key.(string); ok {
			if counter, ok := value.(*atomic.Int64); ok {
				if count := counter.Load(); count > 0 {
					items = append(items, metricItem{Key: k, Count: count})
				}
			}
		}
		return true
	})
	sort.Slice(items, func(i, j int) bool { return items[i].Key < items[j].Key })
	return items
}

// formatPercentage calculates and formats a percentage string.
func formatPercentage(value, total int64) string {
	if total == 0 {
		return "0.00%"
	}
	return fmt.Sprintf("%.2f%%", (float64(value)/float64(total))*100)
}

// ChainMetric holds the calculated metrics for a single behavioral chain.
type ChainMetric struct {
	Name        string
	Completions int64
	Hits        int64
	Resets      int64
}

// GeneralMetric holds a single calculated general metric.
type GeneralMetric struct {
	Name  string
	Value int64
}

// MetricsSummaryData holds all the calculated data for a metrics summary report.
type MetricsSummaryData struct {
	LogSource            string
	ElapsedTime          time.Duration
	LinesProcessed       int64
	LinesPerSecond       float64
	TotalChainsCompleted int64
	TotalChainsReset     int64
	TotalMatchKeyHits    int64
	GeneralMetrics       []GeneralMetric
	ChainMetrics         []ChainMetric
	MatchKeyHitsMetrics  []metricItem
	BlockDurationMetrics []struct {
		Duration time.Duration
		Count    int64
	}
	CmdsPerBlockerMetrics []metricItem
	SkipsByReasonMetrics  []metricItem
	GoodActorHitsMetrics  []metricItem
	TopActorsPerChain     map[string]map[string]*store.ActorStats
	TopN                  int
	BlockActionsTriggered int64
	LogActionsTriggered   int64
}

// collectMetricsSummaryData gathers all metrics from the processor and returns them in a structured format.
func collectMetricsSummaryData(p *Processor, elapsedTime time.Duration, filterTag string) *MetricsSummaryData {
	data := &MetricsSummaryData{
		ElapsedTime:       elapsedTime,
		LinesProcessed:    p.Metrics.LinesProcessed.Load(),
		TopActorsPerChain: p.TopActorsPerChain,
		TopN:              p.TopN,
	}

	if p.LogPath != "" {
		data.LogSource = p.LogPath
	} else {
		data.LogSource = "stdin"
	}

	// --- Chain Metrics ---
	p.Metrics.ChainsCompleted.Range(func(key, value interface{}) bool {
		chainName, _ := key.(string)
		completedCounter, _ := value.(*atomic.Int64)
		completions := completedCounter.Load()

		var hits int64
		if hitsVal, ok := p.Metrics.ChainsHits.Load(chainName); ok {
			if hitsCounter, ok := hitsVal.(*atomic.Int64); ok {
				hits = hitsCounter.Load()
			}
		}

		var resets int64
		if resetVal, ok := p.Metrics.ChainsReset.Load(chainName); ok {
			if resetCounter, ok := resetVal.(*atomic.Int64); ok {
				resets = resetCounter.Load()
			}
		}

		if completions > 0 || resets > 0 || hits > 0 {
			data.ChainMetrics = append(data.ChainMetrics, ChainMetric{Name: chainName, Completions: completions, Hits: hits, Resets: resets})
			data.TotalChainsCompleted += completions
			data.TotalChainsReset += resets
		}
		return true
	})
	sort.Slice(data.ChainMetrics, func(i, j int) bool { return data.ChainMetrics[i].Name < data.ChainMetrics[j].Name })

	// --- General Metrics ---
	generalMetricsSource := []struct {
		Name    string
		Counter *atomic.Int64
		Show    bool
	}{
		{"Valid Hits", &p.Metrics.ValidHits, true},
		{"Parse Errors", &p.Metrics.ParseErrors, true},
		{"Good Actors Skipped", &p.Metrics.GoodActorsSkipped, true},
		{"Reordered Entries", &p.Metrics.ReorderedEntries, true},
		{"Actors Cleaned", &p.Metrics.ActorsCleaned, true},
		{"Blocker Commands Queued", &p.Metrics.BlockerCmdsQueued, filterTag == "metric"},
		{"Blocker Commands Dropped", &p.Metrics.BlockerCmdsDropped, filterTag == "metric"},
		{"Blocker Commands Executed", &p.Metrics.BlockerCmdsExecuted, filterTag == "metric"},
		{"Blocker Retries", &p.Metrics.BlockerRetries, filterTag == "metric"},
	}
	for _, metric := range generalMetricsSource {
		if metric.Show {
			data.GeneralMetrics = append(data.GeneralMetrics, GeneralMetric{Name: metric.Name, Value: metric.Counter.Load()})
		}
	}

	// --- Other Metrics ---
	data.BlockActionsTriggered = p.Metrics.BlockActions.Load()
	data.LogActionsTriggered = p.Metrics.LogActions.Load()
	return data
}

// logMetricsSummary calculates and logs a summary of all application metrics.
// It is a generic function that can be used in different contexts (e.g., dry-run, periodic live summary).
//
// Parameters:
//   - p: The Processor containing the metrics.
//   - elapsedTime: The duration over which the metrics were collected.
//   - logFunc: The logging function to use for output.
//   - logTag: The tag to use for each log line (e.g., "METRICS").
//   - filterTag: The struct tag to filter which general metrics to display (e.g., "dryrun").
func logMetricsSummary(p *Processor, elapsedTime time.Duration, logFunc func(logging.LogLevel, string, string, ...interface{}), logTag, filterTag string) {
	data := collectMetricsSummaryData(p, elapsedTime, filterTag)

	// --- Log Source ---
	logFunc(logging.LevelInfo, logTag, "Log Source: %s", data.LogSource)

	logFunc(logging.LevelInfo, logTag, "Lines Processed: %d", data.LinesProcessed)
	for _, metric := range data.GeneralMetrics {
		logFunc(logging.LevelInfo, logTag, "%s: %d (%s)", metric.Name, metric.Value, formatPercentage(metric.Value, data.LinesProcessed))
	}

	logFunc(logging.LevelInfo, logTag, "Actions Triggered: Block: %d, Log: %d", data.BlockActionsTriggered, data.LogActionsTriggered)
	logFunc(logging.LevelInfo, logTag, "Chains Completed: %d", data.TotalChainsCompleted)
	logFunc(logging.LevelInfo, logTag, "Chains Reset: %d", data.TotalChainsReset)

	logFunc(logging.LevelInfo, logTag, "Time Elapsed: %v", elapsedTime.Round(time.Second))

	if elapsedTime.Seconds() > 0 {
		data.LinesPerSecond = float64(data.LinesProcessed) / elapsedTime.Seconds()
		logFunc(logging.LevelInfo, logTag, "Rate: %.2f lines/sec", data.LinesPerSecond)
	} else {
		logFunc(logging.LevelInfo, logTag, "Rate: n/a (run too fast)")
	}

	// --- Log Commands per Blocker ---
	cmdsPerBlockerMetrics := collectMetricsFromMap(p.Metrics.CmdsPerBlocker)

	// --- Log Block Duration Hits ---
	var blockDurationMetrics []struct { // Keep this separate due to time.Duration key
		Duration time.Duration
		Count    int64
	}
	p.Metrics.BlockDurations.Range(func(key, value interface{}) bool {
		duration, _ := key.(time.Duration)
		counter, _ := value.(*atomic.Int64)
		if count := counter.Load(); count > 0 {
			blockDurationMetrics = append(blockDurationMetrics, struct {
				Duration time.Duration
				Count    int64
			}{duration, count})
		}
		return true
	})

	// --- Log Skips by Reason ---
	skipsByReasonMetrics := collectMetricsFromMap(p.Metrics.SkipsByReason)

	// --- Log MatchKey Hits ---
	matchKeyHitsMetrics := collectMetricsFromMap(p.Metrics.MatchKeyHits)
	var totalMatchKeyHits int64
	for _, item := range matchKeyHitsMetrics {
		totalMatchKeyHits += item.Count
	}

	// --- Log Good Actor Hits ---
	goodActorHitsMetrics := collectMetricsFromMap(p.Metrics.GoodActorHits)

	logFunc(logging.LevelInfo, logTag, "--- Match Key Hits (Total: %d) ---", totalMatchKeyHits)
	for _, metric := range matchKeyHitsMetrics {
		logFunc(logging.LevelInfo, logTag, "  - %s: %d (%s)", metric.Key, metric.Count, formatPercentage(metric.Count, totalMatchKeyHits))
	}

	if len(cmdsPerBlockerMetrics) > 0 {
		logFunc(logging.LevelInfo, logTag, "--- Commands Sent per Blocker ---")
		for _, metric := range cmdsPerBlockerMetrics {
			logFunc(logging.LevelInfo, logTag, "  - %s: %d", metric.Key, metric.Count)
		}
	}

	if len(skipsByReasonMetrics) > 0 {
		logFunc(logging.LevelInfo, logTag, "--- Skips by Reason ---")
		for _, metric := range skipsByReasonMetrics {
			logFunc(logging.LevelInfo, logTag, "  - %s: %d", metric.Key, metric.Count)
		}
	}

	if len(goodActorHitsMetrics) > 0 {
		logFunc(logging.LevelInfo, logTag, "--- Good Actor Hits ---")
		for _, metric := range goodActorHitsMetrics {
			logFunc(logging.LevelInfo, logTag, "  - %s: %d", metric.Key, metric.Count)
		}
	}

	if len(blockDurationMetrics) > 0 {
		logFunc(logging.LevelInfo, logTag, "--- Block Durations Triggered ---")
		sort.Slice(blockDurationMetrics, func(i, j int) bool { return blockDurationMetrics[i].Duration < blockDurationMetrics[j].Duration })
		for _, metric := range blockDurationMetrics {
			logFunc(logging.LevelInfo, logTag, "  - %s: %d", utils.FormatDuration(metric.Duration), metric.Count)
		}
	}

	validHits := p.Metrics.ValidHits.Load()

	// Log the consolidated per-chain breakdown.
	if len(data.ChainMetrics) > 0 {
		logFunc(logging.LevelInfo, logTag, "--- Per-Chain Metrics ---")
		for _, metric := range data.ChainMetrics {
			if validHits > 0 {
				hitsPctStr := formatPercentage(metric.Hits, validHits)
				completionsPctStr := formatPercentage(metric.Completions, data.TotalChainsCompleted)
				resetsPctStr := formatPercentage(metric.Resets, data.TotalChainsReset)
				logFunc(logging.LevelInfo, logTag, "  - %s: Hits: %d (%s), Completed: %d (%s), Resets: %d (%s)",
					metric.Name, metric.Hits, hitsPctStr, metric.Completions, completionsPctStr, metric.Resets, resetsPctStr)
			} else {
				logFunc(logging.LevelInfo, logTag, "  - %s: Hits: %d, Completed: %d, Resets: %d",
					metric.Name, metric.Hits, metric.Completions, metric.Resets)
			}
		}
	}

	// Finally, log the top actors summary if enabled.
	logTopActorsSummary(p, logFunc)
}

// cleanupTopActors is a background goroutine that periodically cleans the TopActorsPerChain map
// to prevent unbounded memory growth in live mode when TopN is enabled.
func cleanupTopActors(p *Processor, stop <-chan struct{}) {
	if p.TopN <= 0 {
		p.LogFunc(logging.LevelDebug, "CLEANUP_TOPN", "Top-N cleanup routine is disabled (top-n <= 0).")
		return // Cleanup is disabled.
	}

	p.ConfigMutex.RLock()
	cleanupInterval := p.Config.CleanupInterval
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
			p.Metrics.LinesProcessed.Add(1)
		}
	}
}
