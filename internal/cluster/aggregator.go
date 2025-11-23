package cluster

import (
	"bot-detector/internal/server"
	"time"
)

// NodeHealthStatus represents the health state of a node in the cluster.
type NodeHealthStatus string

const (
	NodeHealthy NodeHealthStatus = "healthy"
	NodeStale   NodeHealthStatus = "stale"
	NodeError   NodeHealthStatus = "error"
)

// NodeMetricsInfo contains metrics and metadata for a single node.
type NodeMetricsInfo struct {
	NodeName          string                  `json:"node_name"`
	Address           string                  `json:"address"`
	Status            NodeHealthStatus        `json:"status"`
	LastCollected     time.Time               `json:"last_collected"`
	LastError         string                  `json:"last_error,omitempty"`
	ConsecutiveErrors int                     `json:"consecutive_errors"`
	Metrics           *server.MetricsSnapshot `json:"metrics,omitempty"`
}

// AggregatedMetrics contains cluster-wide aggregated metrics and per-node breakdown.
type AggregatedMetrics struct {
	Timestamp    time.Time              `json:"timestamp"`
	TotalNodes   int                    `json:"total_nodes"`
	HealthyNodes int                    `json:"healthy_nodes"`
	StaleNodes   int                    `json:"stale_nodes"`
	ErrorNodes   int                    `json:"error_nodes"`
	Aggregated   server.MetricsSnapshot `json:"aggregated"`
	Nodes        []NodeMetricsInfo      `json:"nodes"`
}

// determineNodeHealth determines the health status of a node based on its metrics.
func determineNodeHealth(collected *CollectedMetrics, staleThreshold time.Duration) NodeHealthStatus {
	// If there are consecutive errors, node is in error state
	if collected.ConsecutiveErrors > 0 {
		return NodeError
	}

	// If no snapshot available yet, node is in error state
	if collected.Snapshot == nil {
		return NodeError
	}

	// If last collection was too long ago, node is stale
	timeSinceCollection := time.Since(collected.LastCollected)
	if timeSinceCollection > staleThreshold {
		return NodeStale
	}

	return NodeHealthy
}

// sumInt64Maps adds all values from src into dst, creating keys if needed.
func sumInt64Maps(dst, src map[string]int64) {
	for key, value := range src {
		dst[key] += value
	}
}

// sumProcessingStats adds processing stats from src into dst.
func sumProcessingStats(dst, src *server.ProcessingStats) {
	dst.LinesProcessed += src.LinesProcessed
	dst.EntriesChecked += src.EntriesChecked
	dst.ParseErrors += src.ParseErrors
	dst.ReorderedLines += src.ReorderedLines
	dst.TimeElapsed += src.TimeElapsed
	// LinesPerSecond is recalculated later based on total lines and total time
}

// sumActorStats adds actor stats from src into dst.
func sumActorStats(dst, src *server.ActorStats) {
	dst.GoodActorsSkipped += src.GoodActorsSkipped
	dst.ActorsCleaned += src.ActorsCleaned
}

// sumChainStats adds chain stats from src into dst.
func sumChainStats(dst, src *server.ChainStats) {
	dst.ActionsBlock += src.ActionsBlock
	dst.ActionsLog += src.ActionsLog
	dst.TotalHits += src.TotalHits
	dst.Completed += src.Completed
	dst.Resets += src.Resets
}

// sumChainMetrics adds per-chain metrics from src into dst.
func sumChainMetrics(dst, src map[string]server.ChainMetric) {
	for chainName, srcMetric := range src {
		dstMetric := dst[chainName]
		dstMetric.Hits += srcMetric.Hits
		dstMetric.Completed += srcMetric.Completed
		dstMetric.Resets += srcMetric.Resets
		dst[chainName] = dstMetric
	}
}

// AggregateMetrics takes collected metrics from all nodes and produces an aggregated view.
// The staleThreshold parameter determines when a node is considered stale (typically 2-3x the poll interval).
func AggregateMetrics(collectedMetrics map[string]*CollectedMetrics, nodes []NodeConfig, staleThreshold time.Duration) *AggregatedMetrics {
	result := &AggregatedMetrics{
		Timestamp:  time.Now(),
		TotalNodes: len(nodes),
		Nodes:      make([]NodeMetricsInfo, 0, len(nodes)),
		Aggregated: server.MetricsSnapshot{
			Timestamp:       time.Now(),
			ProcessingStats: server.ProcessingStats{},
			ActorStats:      server.ActorStats{},
			ChainStats:      server.ChainStats{},
			GoodActorHits:   make(map[string]int64),
			SkipsByReason:   make(map[string]int64),
			MatchKeyHits:    make(map[string]int64),
			BlockDurations:  make(map[string]int64),
			PerChainMetrics: make(map[string]server.ChainMetric),
		},
	}

	// Create a map of node names to addresses for quick lookup
	nodeAddresses := make(map[string]string, len(nodes))
	for _, node := range nodes {
		nodeAddresses[node.Name] = node.Address
	}

	// Process each node's metrics
	for nodeName, collected := range collectedMetrics {
		// Determine node health
		status := determineNodeHealth(collected, staleThreshold)

		// Update health counters
		switch status {
		case NodeHealthy:
			result.HealthyNodes++
		case NodeStale:
			result.StaleNodes++
		case NodeError:
			result.ErrorNodes++
		}

		// Create node info
		nodeInfo := NodeMetricsInfo{
			NodeName:          nodeName,
			Address:           nodeAddresses[nodeName],
			Status:            status,
			LastCollected:     collected.LastCollected,
			LastError:         collected.LastError,
			ConsecutiveErrors: collected.ConsecutiveErrors,
			Metrics:           collected.Snapshot,
		}
		result.Nodes = append(result.Nodes, nodeInfo)

		// Only aggregate metrics from nodes with valid snapshots
		if collected.Snapshot != nil {
			snapshot := collected.Snapshot

			// Sum processing stats
			sumProcessingStats(&result.Aggregated.ProcessingStats, &snapshot.ProcessingStats)

			// Sum actor stats
			sumActorStats(&result.Aggregated.ActorStats, &snapshot.ActorStats)

			// Sum chain stats
			sumChainStats(&result.Aggregated.ChainStats, &snapshot.ChainStats)

			// Sum maps
			if snapshot.GoodActorHits != nil {
				sumInt64Maps(result.Aggregated.GoodActorHits, snapshot.GoodActorHits)
			}
			if snapshot.SkipsByReason != nil {
				sumInt64Maps(result.Aggregated.SkipsByReason, snapshot.SkipsByReason)
			}
			if snapshot.MatchKeyHits != nil {
				sumInt64Maps(result.Aggregated.MatchKeyHits, snapshot.MatchKeyHits)
			}
			if snapshot.BlockDurations != nil {
				sumInt64Maps(result.Aggregated.BlockDurations, snapshot.BlockDurations)
			}
			if snapshot.PerChainMetrics != nil {
				sumChainMetrics(result.Aggregated.PerChainMetrics, snapshot.PerChainMetrics)
			}
		}
	}

	// Recalculate lines per second based on aggregated totals
	if result.Aggregated.ProcessingStats.TimeElapsed > 0 {
		result.Aggregated.ProcessingStats.LinesPerSecond =
			float64(result.Aggregated.ProcessingStats.LinesProcessed) / result.Aggregated.ProcessingStats.TimeElapsed
	}

	return result
}
