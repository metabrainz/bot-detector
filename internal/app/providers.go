package app

import (
	"bot-detector/internal/cluster"
	"bot-detector/internal/logging"
	"bot-detector/internal/server"
	"bot-detector/internal/store"
	"bot-detector/internal/types"
	"bot-detector/internal/utils"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// --- MetricsProvider Interface Implementation ---

// GetConfigForArchive safely retrieves the main config content and its dependencies for archiving.
func (p *Processor) GetConfigForArchive() ([]byte, time.Time, map[string]*types.FileDependency, string, error) {
	p.ConfigMutex.RLock()
	defer p.ConfigMutex.RUnlock()

	// Create a deep copy of the dependencies to avoid race conditions if the config is reloaded
	// while the archive is being generated in a goroutine.
	depsCopy := make(map[string]*types.FileDependency)
	for path, dep := range p.Config.FileDependencies {
		// We only include files that are currently loaded and exist.
		if dep.CurrentStatus != nil && dep.CurrentStatus.Status == types.FileStatusLoaded {
			depsCopy[path] = dep.Clone()
		}
	}

	return p.Config.YAMLContent, p.Config.LastModTime, depsCopy, p.ConfigDir, nil
}

// GetListenAddr returns the HTTP listen address from the config.
// Deprecated: Use GetListenConfigs() instead.
func (p *Processor) GetListenAddr() string {
	if len(p.ListenConfigs) > 0 {
		return p.ListenConfigs[0].Address
	}
	return ""
}

// GetListenConfigs returns all configured listen addresses.
func (p *Processor) GetListenConfigs() interface{} {
	return p.ListenConfigs
}

// GetShutdownChannel returns the channel used for shutdown signals.
func (p *Processor) GetShutdownChannel() chan os.Signal {
	return p.SignalCh
}

// Log is a wrapper around the processor's LogFunc to satisfy the interface.
func (p *Processor) Log(level logging.LogLevel, tag string, format string, v ...interface{}) {
	p.LogFunc(level, tag, format, v...)
}

// GetNodeStatus returns the cluster status of this node.
func (p *Processor) GetNodeStatus() server.NodeStatus {
	return server.NodeStatus{
		Role:          p.NodeRole,
		Name:          p.NodeName,
		Address:       p.NodeAddress,
		LeaderAddress: p.NodeLeaderAddress,
	}
}

// GetMetricsSnapshot returns a JSON-serializable snapshot of current metrics.
func (p *Processor) GetMetricsSnapshot() server.MetricsSnapshot {
	elapsed := time.Since(p.StartTime).Seconds()
	linesProcessed := p.Metrics.LinesProcessed.Load()

	var linesPerSecond float64
	if elapsed > 0 {
		linesPerSecond = float64(linesProcessed) / elapsed
	}

	// Calculate per-chain metrics first so we can compute totals
	perChainMetrics := extractPerChainMetrics(
		p.Metrics.ChainsHits,
		p.Metrics.ChainsCompleted,
		p.Metrics.ChainsReset,
	)

	// Calculate total hits, completed, and resets from per-chain metrics
	var totalHits, totalCompleted, totalResets int64
	for _, metric := range perChainMetrics {
		totalHits += metric.Hits
		totalCompleted += metric.Completed
		totalResets += metric.Resets
	}

	snapshot := server.MetricsSnapshot{
		Timestamp: time.Now(),
		ProcessingStats: server.ProcessingStats{
			LinesProcessed: linesProcessed,
			ValidHits:      p.Metrics.ValidHits.Load(),
			ParseErrors:    p.Metrics.ParseErrors.Load(),
			ReorderedLines: p.Metrics.ReorderedEntries.Load(),
			TimeElapsed:    elapsed,
			LinesPerSecond: linesPerSecond,
		},
		ActorStats: server.ActorStats{
			GoodActorsSkipped: p.Metrics.GoodActorsSkipped.Load(),
			ActorsCleaned:     p.Metrics.ActorsCleaned.Load(),
		},
		ChainStats: server.ChainStats{
			ActionsBlock: p.Metrics.BlockActions.Load(),
			ActionsLog:   p.Metrics.LogActions.Load(),
			TotalHits:    totalHits,
			Completed:    totalCompleted,
			Resets:       totalResets,
		},
		GoodActorHits:   extractSyncMapInt64(p.Metrics.GoodActorHits),
		SkipsByReason:   extractSyncMapInt64(p.Metrics.SkipsByReason),
		MatchKeyHits:    extractSyncMapInt64(p.Metrics.MatchKeyHits),
		BlockDurations:  extractSyncMapInt64(p.Metrics.BlockDurations),
		PerChainMetrics: perChainMetrics,
	}

	return snapshot
}

// GetAggregatedMetrics returns cluster-wide aggregated metrics (leader only).
// Returns nil if this node is not a leader or if the metrics collector is not available.
func (p *Processor) GetAggregatedMetrics() interface{} {
	// Only leaders have a metrics collector
	if p.MetricsCollector == nil {
		return nil
	}

	// Only leaders should aggregate metrics
	if p.NodeRole != "leader" {
		return nil
	}

	// Get collected metrics from all nodes
	collectedMetrics := p.MetricsCollector.GetCollectedMetrics()

	// Determine stale threshold (3x the poll interval is a reasonable default)
	var staleThreshold time.Duration
	if p.Cluster != nil && p.Cluster.MetricsReportInterval > 0 {
		staleThreshold = p.Cluster.MetricsReportInterval * 3
	} else {
		staleThreshold = 180 * time.Second // 3 minutes default
	}

	// Get the nodes list from cluster config
	var nodes []cluster.NodeConfig
	if p.Cluster != nil {
		nodes = p.Cluster.Nodes
	}

	// Aggregate metrics using the cluster aggregator
	aggregated := cluster.AggregateMetrics(collectedMetrics, nodes, staleThreshold)

	return aggregated
}

// extractSyncMapInt64 extracts a sync.Map of string->*atomic.Int64 into a regular map.
func extractSyncMapInt64(m *sync.Map) map[string]int64 {
	result := make(map[string]int64)
	if m == nil {
		return result
	}

	m.Range(func(key, value interface{}) bool {
		if k, ok := key.(string); ok {
			if v, ok := value.(*atomic.Int64); ok {
				result[k] = v.Load()
			}
		}
		return true
	})

	return result
}

// extractPerChainMetrics extracts per-chain metrics from sync.Maps.
func extractPerChainMetrics(hits, completed, resets *sync.Map) map[string]server.ChainMetric {
	result := make(map[string]server.ChainMetric)

	// Collect all chain names from hits map
	chainNames := make(map[string]bool)
	if hits != nil {
		hits.Range(func(key, _ interface{}) bool {
			if k, ok := key.(string); ok {
				chainNames[k] = true
			}
			return true
		})
	}

	// Build metrics for each chain
	for chainName := range chainNames {
		metric := server.ChainMetric{}

		if hits != nil {
			if v, ok := hits.Load(chainName); ok {
				if counter, ok := v.(*atomic.Int64); ok {
					metric.Hits = counter.Load()
				}
			}
		}

		if completed != nil {
			if v, ok := completed.Load(chainName); ok {
				if counter, ok := v.(*atomic.Int64); ok {
					metric.Completed = counter.Load()
				}
			}
		}

		if resets != nil {
			if v, ok := resets.Load(chainName); ok {
				if counter, ok := v.(*atomic.Int64); ok {
					metric.Resets = counter.Load()
				}
			}
		}

		result[chainName] = metric
	}

	return result
}

// --- StoreProvider Interface Implementation ---

func (p *Processor) GetCleanupInterval() time.Duration {
	return p.Config.Checker.ActorCleanupInterval
}

func (p *Processor) GetIdleTimeout() time.Duration {
	return p.Config.Checker.ActorStateIdleTimeout
}

func (p *Processor) GetMaxTimeSinceLastHit() time.Duration {
	return p.Config.Checker.MaxTimeSinceLastHit
}

func (p *Processor) GetTopN() int {
	return p.TopN
}

func (p *Processor) GetTopActorsPerChain() map[string]map[string]*store.ActorStats {
	return p.TopActorsPerChain
}

func (p *Processor) GetActivityStore() map[store.Actor]*store.ActorActivity {
	return p.ActivityStore
}

func (p *Processor) GetActivityMutex() *sync.RWMutex {
	return p.ActivityMutex
}

func (p *Processor) GetNodeName() string {
	return p.NodeName
}

func (p *Processor) GetNodeRole() string {
	return p.NodeRole
}

func (p *Processor) GetNodeLeaderAddress() string {
	return p.NodeLeaderAddress
}

func (p *Processor) GetClusterNodes() interface{} {
	if p.Cluster == nil {
		return nil
	}
	return p.Cluster.Nodes
}

func (p *Processor) GetClusterProtocol() string {
	if p.Cluster == nil {
		return "http"
	}
	return p.Cluster.Protocol
}

func (p *Processor) GetTestSignals() *store.TestSignals {
	if p.TestSignals == nil {
		return nil
	}
	// Convert main.TestSignals to store.TestSignals
	return &store.TestSignals{
		CleanupDoneSignal: p.TestSignals.CleanupDoneSignal,
	}
}

func (p *Processor) IncrementActorsCleaned() {
	p.Metrics.ActorsCleaned.Add(1)
}

// --- MetricsProvider Interface Implementation ---

func (p *Processor) IncrementBlockerCmdsQueued() {
	p.Metrics.BlockerCmdsQueued.Add(1)
}

func (p *Processor) IncrementBlockerCmdsDropped() {
	p.Metrics.BlockerCmdsDropped.Add(1)
}

func (p *Processor) IncrementBlockerCmdsExecuted() {
	p.Metrics.BlockerCmdsExecuted.Add(1)
}

// --- HAProxyProvider Interface Implementation ---

func (p *Processor) GetBlockerAddresses() []string {
	return p.Config.Blockers.Backends.HAProxy.Addresses
}

func (p *Processor) GetDurationTables() map[time.Duration]string {
	return p.Config.Blockers.Backends.HAProxy.DurationTables
}

func (p *Processor) GetBlockTableNameFallback() string {
	return p.Config.Blockers.Backends.HAProxy.TableNameFallback
}

func (p *Processor) GetBlockerMaxRetries() int {
	return p.Config.Blockers.MaxRetries
}

func (p *Processor) GetBlockerRetryDelay() time.Duration {
	return p.Config.Blockers.RetryDelay
}

func (p *Processor) GetBlockerDialTimeout() time.Duration {
	return p.Config.Blockers.DialTimeout
}

func (p *Processor) GetMaxCommandsPerBatch() int {
	return p.Config.Blockers.MaxCommandsPerBatch
}

func (p *Processor) IncrementBlockerRetries() {
	p.Metrics.BlockerRetries.Add(1)
}

func (p *Processor) IncrementCmdsPerBlocker(addr string) {
	if p.Metrics == nil || p.Metrics.CmdsPerBlocker == nil {
		return
	}
	val, _ := p.Metrics.CmdsPerBlocker.LoadOrStore(addr, new(atomic.Int64))
	val.(*atomic.Int64).Add(1)
}

// GenerateHTMLMetricsReport creates the full metrics report as an HTML-safe string.
func (p *Processor) GenerateHTMLMetricsReport() string {
	var report strings.Builder
	webLogFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		// Sanitize the formatted string before writing it to the HTML report.
		report.WriteString(utils.ForHTML(fmt.Sprintf(format, args...)) + "\n")
	}
	LogMetricsSummary(p, time.Since(p.StartTime), webLogFunc, "METRICS", "metric")
	return report.String()
}

// GenerateStepsMetricsReport creates a report of step execution counts as an HTML-safe string.
func (p *Processor) GenerateStepsMetricsReport() string {
	var report strings.Builder
	report.WriteString("--- Step Execution Counts ---\n")
	if p.Metrics.StepExecutionCounts == nil {
		report.WriteString("Step metrics are not enabled or initialized.\n")
		return report.String()
	}

	// Collect and sort step metrics for consistent output
	type StepMetric struct {
		Name  string
		Count int64
	}
	var stepMetrics []StepMetric
	p.Metrics.StepExecutionCounts.Range(func(key, value interface{}) bool {
		stepName, _ := key.(string)
		count, _ := value.(*atomic.Int64)
		stepMetrics = append(stepMetrics, StepMetric{Name: stepName, Count: count.Load()})
		return true
	})

	sort.Slice(stepMetrics, func(i, j int) bool {
		if stepMetrics[i].Count == stepMetrics[j].Count {
			return stepMetrics[i].Name < stepMetrics[j].Name
		}
		return stepMetrics[i].Count >= stepMetrics[j].Count
	})

	for _, sm := range stepMetrics {
		report.WriteString(fmt.Sprintf("%12d %s\n", sm.Count, utils.ForHTML(sm.Name)))
	}
	return report.String()
}
