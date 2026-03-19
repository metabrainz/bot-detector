package app

import (
	"bot-detector/internal/config"
	"bot-detector/internal/logging"
	"bot-detector/internal/store"
	"bot-detector/internal/utils"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func logTopActorsSummary(p *Processor, logFunc func(logging.LogLevel, string, string, ...interface{})) {
	p.ActivityMutex.RLock()
	defer p.ActivityMutex.RUnlock()
	if p.TopN <= 0 {
		return // Top-N reporting is disabled.
	}

	logFunc(logging.LevelInfo, "STATS", "=== Top %d Actors per Chain ===", p.TopN)
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
		logFunc(logging.LevelInfo, "STATS", config.TopNHeaderFormat, "Hits", "Compl.", "Resets", "Seen", "Actor")

		limit := p.TopN
		for i := 0; i < limit && i < len(stats); i++ {
			stat := stats[i]
			// Only show actors with at least one hit.
			if stat.Stats.Hits == 0 {
				break
			}

			// Parse actor string back to Actor struct to look up LastRequestTime
			actorObj, err := store.NewActorFromString(stat.Actor)
			lastSeen := "n/a"
			if err == nil {
				if activity, ok := p.ActivityStore[actorObj]; ok {
					lastSeen = formatLastSeen(activity.LastRequestTime, p.NowFunc())
				}
			}

			logFunc(logging.LevelInfo, "STATS", config.TopNRowFormat,
				stat.Stats.Hits, stat.Stats.Completions, stat.Stats.Resets, lastSeen, stat.Actor)
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

	// In multi-website mode, show website count instead of single log path
	if len(p.Websites) > 0 {
		data.LogSource = fmt.Sprintf("%d websites", len(p.Websites))
	} else if p.LogPath != "" {
		data.LogSource = p.LogPath
	} else {
		data.LogSource = "stdin"
	}

	if !p.EnableMetrics { // Added check
		return data // Return early if metrics are disabled
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
		{"Entries Checked", &p.Metrics.EntriesChecked, true},
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

func logGeneralStats(logFunc func(logging.LogLevel, string, string, ...interface{}), logTag string, data *MetricsSummaryData) {
	logFunc(logging.LevelInfo, logTag, "=== General Processing Statistics ===")
	logFunc(logging.LevelInfo, logTag, "Log Source: %s", data.LogSource)
	logFunc(logging.LevelInfo, logTag, "Lines Processed: %d", data.LinesProcessed)
	for _, metric := range data.GeneralMetrics {
		if metric.Name == "Entries Checked" || metric.Name == "Parse Errors" || metric.Name == "Reordered Entries" {
			logFunc(logging.LevelInfo, logTag, "%s: %d (%s)", metric.Name, metric.Value, formatPercentage(metric.Value, data.LinesProcessed))
		}
	}
	logFunc(logging.LevelInfo, logTag, "Time Elapsed: %v", data.ElapsedTime.Round(time.Second))
	if data.ElapsedTime.Seconds() > 0 {
		data.LinesPerSecond = float64(data.LinesProcessed) / data.ElapsedTime.Seconds()
		logFunc(logging.LevelInfo, logTag, "Rate: %.2f lines/sec", data.LinesPerSecond)
	} else {
		logFunc(logging.LevelInfo, logTag, "Rate: n/a (run too fast)")
	}
}

func logActorStats(logFunc func(logging.LogLevel, string, string, ...interface{}), logTag string, data *MetricsSummaryData, skipsByReasonMetrics, goodActorHitsMetrics []metricItem) {
	logFunc(logging.LevelInfo, logTag, "=== Actor Statistics ===")
	for _, metric := range data.GeneralMetrics {
		if metric.Name == "Good Actors Skipped" || metric.Name == "Actors Cleaned" {
			logFunc(logging.LevelInfo, logTag, "%s: %d (%s)", metric.Name, metric.Value, formatPercentage(metric.Value, data.LinesProcessed))
		}
	}
	if len(skipsByReasonMetrics) > 0 {
		logFunc(logging.LevelInfo, logTag, "=== Skips by Reason ===")
		for _, metric := range skipsByReasonMetrics {
			logFunc(logging.LevelInfo, logTag, "  - %s: %d", metric.Key, metric.Count)
		}
	}
	if len(goodActorHitsMetrics) > 0 {
		logFunc(logging.LevelInfo, logTag, "=== Good Actor Matches ===")
		for _, metric := range goodActorHitsMetrics {
			logFunc(logging.LevelInfo, logTag, "  - %s: %d", metric.Key, metric.Count)
		}
	}
}

func logChainAndActionStats(logFunc func(logging.LogLevel, string, string, ...interface{}), logTag string, data *MetricsSummaryData, cmdsPerBlockerMetrics, matchKeyHitsMetrics []metricItem, blockDurationMetrics []struct {
	Duration time.Duration
	Count    int64
}, totalMatchKeyHits int64) {
	logFunc(logging.LevelInfo, logTag, "=== Chain and Action Statistics ===")
	logFunc(logging.LevelInfo, logTag, "Actions Triggered: Block: %d, Log: %d", data.BlockActionsTriggered, data.LogActionsTriggered)
	logFunc(logging.LevelInfo, logTag, "Chains Completed: %d", data.TotalChainsCompleted)
	logFunc(logging.LevelInfo, logTag, "Chains Reset: %d", data.TotalChainsReset)
	for _, metric := range data.GeneralMetrics {
		if metric.Name == "Blocker Commands Queued" || metric.Name == "Blocker Commands Dropped" || metric.Name == "Blocker Commands Executed" || metric.Name == "Blocker Retries" {
			logFunc(logging.LevelInfo, logTag, "%s: %d", metric.Name, metric.Value)
		}
	}
	logFunc(logging.LevelInfo, logTag, "=== Match Key Distribution (Total: %d) ===", totalMatchKeyHits)
	for _, metric := range matchKeyHitsMetrics {
		logFunc(logging.LevelInfo, logTag, "  - %s: %d (%s)", metric.Key, metric.Count, formatPercentage(metric.Count, totalMatchKeyHits))
	}
	if len(blockDurationMetrics) > 0 {
		logFunc(logging.LevelInfo, logTag, "=== Blocks by Duration ===")
		sort.Slice(blockDurationMetrics, func(i, j int) bool { return blockDurationMetrics[i].Duration < blockDurationMetrics[j].Duration })
		for _, metric := range blockDurationMetrics {
			logFunc(logging.LevelInfo, logTag, "  - %s: %d", utils.FormatDuration(metric.Duration), metric.Count)
		}
	}
	if len(cmdsPerBlockerMetrics) > 0 {
		logFunc(logging.LevelInfo, logTag, "=== Commands Sent per Blocker ===")
		for _, metric := range cmdsPerBlockerMetrics {
			logFunc(logging.LevelInfo, logTag, "  - %s: %d", metric.Key, metric.Count)
		}
	}
}

func logPerChainMetrics(p *Processor, logFunc func(logging.LogLevel, string, string, ...interface{}), logTag string, data *MetricsSummaryData) {
	validHits := p.Metrics.EntriesChecked.Load()

	if len(data.ChainMetrics) > 0 {
		// Filter out inactive chains (no matches, completions, or resets)
		var activeMetrics []ChainMetric
		for _, metric := range data.ChainMetrics {
			if metric.Hits > 0 || metric.Completions > 0 || metric.Resets > 0 {
				activeMetrics = append(activeMetrics, metric)
			}
		}

		if len(activeMetrics) == 0 {
			logFunc(logging.LevelInfo, logTag, "=== Per-Chain Metrics ===")
			logFunc(logging.LevelInfo, logTag, "  (No chain activity)")
			return
		}

		logFunc(logging.LevelInfo, logTag, "=== Per-Chain Metrics (%d active) ===", len(activeMetrics))
		for _, metric := range activeMetrics {
			// Find the chain to get website information
			chainName := metric.Name
			var websiteInfo string
			for i := range p.Chains {
				if p.Chains[i].Name == metric.Name {
					if len(p.Chains[i].Websites) == 0 {
						websiteInfo = " [global]"
					} else {
						websiteInfo = fmt.Sprintf(" [%s]", strings.Join(p.Chains[i].Websites, ", "))
					}
					break
				}
			}

			if validHits > 0 {
				hitsPctStr := formatPercentage(metric.Hits, validHits)
				completionsPctStr := formatPercentage(metric.Completions, data.TotalChainsCompleted)
				resetsPctStr := formatPercentage(metric.Resets, metric.Hits)
				logFunc(logging.LevelInfo, logTag, "  - %s%s: Matches: %d (%s), Completed: %d (%s), Resets: %d (%s)",
					chainName, websiteInfo, metric.Hits, hitsPctStr, metric.Completions, completionsPctStr, metric.Resets, resetsPctStr)
			} else {
				logFunc(logging.LevelInfo, logTag, "  - %s%s: Matches: %d, Completed: %d, Resets: %d",
					chainName, websiteInfo, metric.Hits, metric.Completions, metric.Resets)
			}
		}
	}
}

// LogMetricsSummary calculates and logs a summary of all application metrics.
// It is a generic function that can be used in different contexts (e.g., dry-run, periodic live summary).
//
// Parameters:
//   - p: The Processor containing the metrics.
//   - elapsedTime: The duration over which the metrics were collected.
//   - logFunc: The logging function to use for output.
//   - logTag: The tag to use for each log line (e.g., "METRICS").
//   - filterTag: The struct tag to filter which general metrics to display (e.g., "dryrun").
//
// Exported for use in app package.
func LogMetricsSummary(p *Processor, elapsedTime time.Duration, logFunc func(logging.LogLevel, string, string, ...interface{}), logTag, filterTag string) {
	if !p.EnableMetrics {
		logFunc(logging.LevelInfo, logTag, "Metrics are disabled.")
		return
	}

	data := collectMetricsSummaryData(p, elapsedTime, filterTag)

	cmdsPerBlockerMetrics := collectMetricsFromMap(p.Metrics.CmdsPerBlocker)
	var blockDurationMetrics []struct {
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

	skipsByReasonMetrics := collectMetricsFromMap(p.Metrics.SkipsByReason)
	matchKeyHitsMetrics := collectMetricsFromMap(p.Metrics.MatchKeyHits)
	var totalMatchKeyHits int64
	for _, item := range matchKeyHitsMetrics {
		totalMatchKeyHits += item.Count
	}
	goodActorHitsMetrics := collectMetricsFromMap(p.Metrics.GoodActorHits)

	logGeneralStats(logFunc, logTag, data)
	logActorStats(logFunc, logTag, data, skipsByReasonMetrics, goodActorHitsMetrics)
	logChainAndActionStats(logFunc, logTag, data, cmdsPerBlockerMetrics, matchKeyHitsMetrics, blockDurationMetrics, totalMatchKeyHits)
	logPerChainMetrics(p, logFunc, logTag, data)
	logTopActorsSummary(p, logFunc)
}

// cleanupTopActors is a background goroutine that periodically cleans the TopActorsPerChain map
// to prevent unbounded memory growth in live mode when TopN is enabled.
func formatLastSeen(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	duration := now.Sub(t)

	// If the event is in the future relative to 'now', take the absolute value
	// for "last seen" to represent magnitude.
	if duration < 0 {
		duration = -duration
	}

	if duration.Hours() >= 24 {
		days := int(duration.Hours() / 24)
		return fmt.Sprintf("%dd", days)
	} else if duration.Hours() >= 1 {
		hours := int(duration.Hours())
		return fmt.Sprintf("%dh", hours)
	} else if duration.Minutes() >= 1 {
		minutes := int(duration.Minutes())
		return fmt.Sprintf("%dm", minutes)
	} else {
		seconds := int(duration.Seconds())
		return fmt.Sprintf("%ds", seconds)
	}
}
