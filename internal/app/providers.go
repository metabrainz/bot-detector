package app

import (
	"bot-detector/internal/cluster"
	"bot-detector/internal/logging"
	"bot-detector/internal/metrics"
	"bot-detector/internal/persistence"
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
			EntriesChecked: p.Metrics.EntriesChecked.Load(),
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
		WebsiteMetrics:  extractWebsiteMetrics(p.Metrics),
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

// extractWebsiteMetrics extracts per-website metrics from the Metrics struct.
func extractWebsiteMetrics(m *metrics.Metrics) map[string]server.WebsiteMetric {
	result := make(map[string]server.WebsiteMetric)
	if m == nil {
		return result
	}

	// Collect all website names
	websiteNames := make(map[string]bool)
	if m.WebsiteLinesParsed != nil {
		m.WebsiteLinesParsed.Range(func(key, _ interface{}) bool {
			if k, ok := key.(string); ok {
				websiteNames[k] = true
			}
			return true
		})
	}

	// Build metrics for each website
	for website := range websiteNames {
		metric := server.WebsiteMetric{}

		if v, ok := m.WebsiteLinesParsed.Load(website); ok {
			if counter, ok := v.(*atomic.Int64); ok {
				metric.LinesParsed = counter.Load()
			}
		}
		if v, ok := m.WebsiteChainsMatched.Load(website); ok {
			if counter, ok := v.(*atomic.Int64); ok {
				metric.ChainsMatched = counter.Load()
			}
		}
		if v, ok := m.WebsiteChainsReset.Load(website); ok {
			if counter, ok := v.(*atomic.Int64); ok {
				metric.ChainsReset = counter.Load()
			}
		}
		if v, ok := m.WebsiteChainsComplete.Load(website); ok {
			if counter, ok := v.(*atomic.Int64); ok {
				metric.ChainsCompleted = counter.Load()
			}
		}

		result[website] = metric
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

func (p *Processor) GetBlocker() interface{} {
	return p.Blocker
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

// GetPersistenceState returns the persistence state for an IP (if exists).
func (p *Processor) GetPersistenceState(ip string) (interface{}, bool) {
	if !p.PersistenceEnabled {
		return nil, false
	}
	p.PersistenceMutex.Lock()
	defer p.PersistenceMutex.Unlock()
	state, err := persistence.GetIPState(p.DB, ip)
	if err != nil || state == nil {
		return nil, false
	}
	return *state, true
}

// RemoveFromPersistence removes an IP from persistence state, bad actors, and scores.
func (p *Processor) RemoveFromPersistence(ip string) error {
	if !p.PersistenceEnabled {
		return nil
	}

	p.PersistenceMutex.Lock()
	defer p.PersistenceMutex.Unlock()

	// Remove from bad actors and scores
	_ = persistence.RemoveBadActor(p.DB, ip)

	// Check if IP exists in ips table
	state, err := persistence.GetIPState(p.DB, ip)
	if err != nil {
		return fmt.Errorf("failed to query IP state: %w", err)
	}
	if state == nil {
		return nil
	}

	if err := persistence.DeleteIPState(p.DB, ip); err != nil {
		return fmt.Errorf("failed to delete IP state: %w", err)
	}

	if err := persistence.InsertEvent(p.DB, time.Now(), persistence.EventTypeUnblock, ip, "manual_clear", 0, ""); err != nil {
		return fmt.Errorf("failed to insert unblock event: %w", err)
	}

	return nil
}

// GetIPStates returns the complete IPStates map for state sync.
func (p *Processor) GetIPStates() map[string]persistence.IPState {
	states, err := persistence.GetAllIPStates(p.DB)
	if err != nil {
		p.LogFunc(logging.LevelError, "PERSISTENCE", "Failed to get all IP states: %v", err)
		return make(map[string]persistence.IPState)
	}
	return states
}

// GetPersistenceMutex returns the mutex for IPStates.
func (p *Processor) GetPersistenceMutex() *sync.Mutex {
	return &p.PersistenceMutex
}

// GetClusterConfig returns the cluster configuration (nil if not in cluster).
func (p *Processor) GetClusterConfig() interface{} {
	return p.Cluster
}

// GetStateSyncConfig returns state sync configuration values.
func (p *Processor) GetStateSyncConfig() (bool, bool, time.Duration, bool) {
	if p.Cluster == nil {
		return false, false, 0, false
	}
	return p.Cluster.StateSync.Enabled,
		p.Cluster.StateSync.Compression,
		p.Cluster.StateSync.Timeout,
		p.Cluster.StateSync.Incremental
}

func (p *Processor) GetStateSyncManager() interface{} {
	return p.StateSyncManager
}

func (p *Processor) GetBadActorInfo(ip string) (interface{}, interface{}) {
	if !p.PersistenceEnabled {
		return nil, nil
	}
	ba, _ := persistence.GetBadActor(p.DB, ip)
	score, _ := persistence.GetScore(p.DB, ip)
	return ba, score
}

func (p *Processor) GetAllBadActors() ([]interface{}, error) {
	if !p.PersistenceEnabled {
		return nil, nil
	}
	actors, err := persistence.GetAllBadActors(p.DB)
	if err != nil {
		return nil, err
	}
	result := make([]interface{}, len(actors))
	for i, a := range actors {
		result[i] = a
	}
	return result, nil
}

func (p *Processor) GetBadActorsThreshold() float64 {
	p.ConfigMutex.RLock()
	defer p.ConfigMutex.RUnlock()
	if !p.Config.BadActors.Enabled {
		return 0
	}
	return p.Config.BadActors.Threshold
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

func (p *Processor) IncrementBackendResyncs() {
	p.Metrics.BackendResyncs.Add(1)
}

func (p *Processor) IncrementBackendRestarts() {
	p.Metrics.BackendRestarts.Add(1)
}

func (p *Processor) IncrementBackendRecoveries() {
	p.Metrics.BackendRecoveries.Add(1)
}

func (p *Processor) IncrementCmdsPerBlocker(addr string) {
	if p.Metrics == nil || p.Metrics.CmdsPerBlocker == nil {
		return
	}
	val, _ := p.Metrics.CmdsPerBlocker.LoadOrStore(addr, new(atomic.Int64))
	val.(*atomic.Int64).Add(1)
}

// GenerateMetricsReport creates the full metrics report as a plain-text string.
func (p *Processor) GenerateMetricsReport() string {
	var report strings.Builder
	report.WriteString(fmt.Sprintf("Generated: %s\n", time.Now().Format(time.RFC3339)))
	report.WriteString(fmt.Sprintf("Uptime: %s\n\n", time.Since(p.StartTime).Round(time.Second)))
	webLogFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
		// Sanitize the formatted string before writing it to the HTML report.
		report.WriteString(utils.ForHTML(fmt.Sprintf(format, args...)) + "\n")
	}
	LogMetricsSummary(p, time.Since(p.StartTime), webLogFunc, "METRICS", "metric")
	return report.String()
}

// GenerateStepsMetricsReport creates a report of step execution counts grouped by website.
func (p *Processor) GenerateStepsMetricsReport() string {
	var report strings.Builder
	report.WriteString(fmt.Sprintf("Generated: %s\n\n", time.Now().Format(time.RFC3339)))
	report.WriteString("=== Step Execution Counts ===\n")
	if p.Metrics.StepExecutionCounts == nil {
		report.WriteString("Step metrics are not enabled or initialized.\n")
		return report.String()
	}

	// Collect step metrics and categorize by website
	type StepMetric struct {
		Name    string
		Website string // extracted from step name (e.g., "[main_site]" or "@vhost")
		Count   int64
	}
	var stepMetrics []StepMetric
	var totalSteps int64

	p.Metrics.StepExecutionCounts.Range(func(key, value interface{}) bool {
		stepName, _ := key.(string)
		count, _ := value.(*atomic.Int64)
		stepCount := count.Load()

		// Extract website from step name (format: "step X/Y of ChainName[website]" or "ChainName@vhost")
		website := "global"
		if idx := strings.Index(stepName, "["); idx != -1 {
			if endIdx := strings.Index(stepName[idx:], "]"); endIdx != -1 {
				website = stepName[idx+1 : idx+endIdx]
			}
		} else if idx := strings.Index(stepName, "@"); idx != -1 {
			website = stepName[idx+1:]
		}

		stepMetrics = append(stepMetrics, StepMetric{Name: stepName, Website: website, Count: stepCount})
		totalSteps += stepCount
		return true
	})

	// Group by website
	websiteSteps := make(map[string][]StepMetric)
	for _, sm := range stepMetrics {
		websiteSteps[sm.Website] = append(websiteSteps[sm.Website], sm)
	}

	// Sort websites (global first, then alphabetically)
	var websites []string
	for ws := range websiteSteps {
		websites = append(websites, ws)
	}
	sort.Slice(websites, func(i, j int) bool {
		if websites[i] == "global" {
			return true
		}
		if websites[j] == "global" {
			return false
		}
		return websites[i] < websites[j]
	})

	report.WriteString(fmt.Sprintf("Total Step Executions: %d\n\n", totalSteps))

	// Report per website
	for _, ws := range websites {
		steps := websiteSteps[ws]

		// Calculate website total
		var websiteTotal int64
		for _, sm := range steps {
			websiteTotal += sm.Count
		}

		// Sort steps by count (descending), then name
		sort.Slice(steps, func(i, j int) bool {
			if steps[i].Count == steps[j].Count {
				return steps[i].Name < steps[j].Name
			}
			return steps[i].Count > steps[j].Count
		})

		if ws == "global" {
			report.WriteString(fmt.Sprintf("=== Global Chains (%d executions) ===\n", websiteTotal))
		} else {
			report.WriteString(fmt.Sprintf("=== Website: %s (%d executions) ===\n", ws, websiteTotal))
		}

		for _, sm := range steps {
			percentage := float64(0)
			if totalSteps > 0 {
				percentage = float64(sm.Count) * 100.0 / float64(totalSteps)
			}
			report.WriteString(fmt.Sprintf("%12d  %6.2f%%  %s\n", sm.Count, percentage, utils.ForHTML(sm.Name)))
		}
		report.WriteString("\n")
	}

	return report.String()
}

// GenerateWebsiteStatsReport creates a report of multi-website statistics.
func (p *Processor) GenerateWebsiteStatsReport() string {
	var report strings.Builder

	// Check if multi-website mode is enabled
	if len(p.Websites) == 0 {
		report.WriteString("Multi-website mode is not enabled.\n")
		report.WriteString("To enable, add a 'websites' section to your config.yaml\n")
		return report.String()
	}

	report.WriteString(fmt.Sprintf("Generated: %s\n\n", time.Now().Format(time.RFC3339)))
	report.WriteString("=== Multi-Website Statistics ===\n\n")

	// Website configuration
	report.WriteString(fmt.Sprintf("Total Websites: %d\n", len(p.Websites)))
	report.WriteString(fmt.Sprintf("Global Chains: %d\n", len(p.GlobalChains)))
	report.WriteString(fmt.Sprintf("Website-Specific Chains: %d\n\n", len(p.WebsiteChains)))

	// List configured websites
	report.WriteString("=== Configured Websites ===\n")
	for _, website := range p.Websites {
		report.WriteString(fmt.Sprintf("  %s:\n", utils.ForHTML(website.Name)))
		report.WriteString(fmt.Sprintf("    VHosts: %s\n", utils.ForHTML(strings.Join(website.VHosts, ", "))))
		report.WriteString(fmt.Sprintf("    Log Path: %s\n", utils.ForHTML(website.LogPath)))

		// Count total chains (global + website-specific)
		totalChains := len(p.GlobalChains)
		if chainIndices, ok := p.WebsiteChains[website.Name]; ok {
			totalChains += len(chainIndices)
		}
		report.WriteString(fmt.Sprintf("    Chains: %d\n", totalChains))

		// Show per-website metrics
		linesParsed := int64(0)
		if val, ok := p.Metrics.WebsiteLinesParsed.Load(website.Name); ok {
			linesParsed = val.(*atomic.Int64).Load()
		}
		chainsMatched := int64(0)
		if val, ok := p.Metrics.WebsiteChainsMatched.Load(website.Name); ok {
			chainsMatched = val.(*atomic.Int64).Load()
		}
		chainsComplete := int64(0)
		if val, ok := p.Metrics.WebsiteChainsComplete.Load(website.Name); ok {
			chainsComplete = val.(*atomic.Int64).Load()
		}
		chainsReset := int64(0)
		if val, ok := p.Metrics.WebsiteChainsReset.Load(website.Name); ok {
			chainsReset = val.(*atomic.Int64).Load()
		}

		report.WriteString(fmt.Sprintf("    Lines Parsed: %d\n", linesParsed))
		report.WriteString(fmt.Sprintf("    Chain Matches: %d\n", chainsMatched))
		report.WriteString(fmt.Sprintf("    Chain Completions: %d\n", chainsComplete))
		report.WriteString(fmt.Sprintf("    Chain Resets: %d\n", chainsReset))
	}

	// Unknown vhosts section
	p.UnknownVHostsMux.Lock()
	unknownCount := len(p.UnknownVHosts)
	var unknownVHosts []string
	for vhost := range p.UnknownVHosts {
		unknownVHosts = append(unknownVHosts, vhost)
	}
	p.UnknownVHostsMux.Unlock()

	report.WriteString("\n=== Unknown VHosts ===\n")
	if unknownCount == 0 {
		report.WriteString("  None (all vhosts are recognized)\n")
	} else {
		sort.Strings(unknownVHosts)
		report.WriteString(fmt.Sprintf("  Total: %d\n", unknownCount))
		report.WriteString("  VHosts:\n")
		for _, vhost := range unknownVHosts {
			report.WriteString(fmt.Sprintf("    - %s\n", utils.ForHTML(vhost)))
		}
		report.WriteString("\n  Note: Unknown vhosts are logged once and their entries are skipped.\n")
		report.WriteString("  To fix: Add the vhost to a website's 'vhosts' list in config.yaml\n")
	}

	return report.String()
}
