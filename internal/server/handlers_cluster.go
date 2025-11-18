package server

import (
	"encoding/json"
	"net/http"
	"time"
)

// clusterStatusHandler returns the current node's cluster status.
// GET /cluster/status
//
// Response format:
//
//	{
//	  "role": "leader",
//	  "name": "node-1",
//	  "address": "localhost:8080"
//	}
//
// Or for a follower:
//
//	{
//	  "role": "follower",
//	  "name": "node-2",
//	  "address": "localhost:9090",
//	  "leader": "node-1:8080"
//	}
func clusterStatusHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get node status from provider
		status := p.GetNodeStatus()

		// Create JSON response with omitempty for optional fields
		type response struct {
			Role          string `json:"role"`
			Name          string `json:"name,omitempty"`
			Address       string `json:"address,omitempty"`
			LeaderAddress string `json:"leader,omitempty"`
		}

		// Convert NodeStatus to response type
		resp := response(status)

		// Set content type
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		// Encode and send response
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			p.Log(3, "CLUSTER", "Failed to encode cluster status response: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	}
}

// MetricsSnapshot represents a snapshot of node metrics in JSON format.
type MetricsSnapshot struct {
	Timestamp       time.Time              `json:"timestamp"`
	ProcessingStats ProcessingStats        `json:"processing_stats"`
	ActorStats      ActorStats             `json:"actor_stats"`
	ChainStats      ChainStats             `json:"chain_stats"`
	GoodActorHits   map[string]int64       `json:"good_actor_hits,omitempty"`
	SkipsByReason   map[string]int64       `json:"skips_by_reason,omitempty"`
	MatchKeyHits    map[string]int64       `json:"match_key_hits,omitempty"`
	BlockDurations  map[string]int64       `json:"block_durations,omitempty"`
	PerChainMetrics map[string]ChainMetric `json:"per_chain_metrics,omitempty"`
}

// ProcessingStats contains general log processing statistics.
type ProcessingStats struct {
	LinesProcessed int64   `json:"lines_processed"`
	ValidHits      int64   `json:"valid_hits"`
	ParseErrors    int64   `json:"parse_errors"`
	ReorderedLines int64   `json:"reordered_lines"`
	TimeElapsed    float64 `json:"time_elapsed_seconds"`
	LinesPerSecond float64 `json:"lines_per_second"`
}

// ActorStats contains statistics about actors (IPs/UAs).
type ActorStats struct {
	GoodActorsSkipped int64 `json:"good_actors_skipped"`
	ActorsCleaned     int64 `json:"actors_cleaned"`
}

// ChainStats contains chain execution statistics.
type ChainStats struct {
	ActionsBlock int64 `json:"actions_block"`
	ActionsLog   int64 `json:"actions_log"`
	TotalHits    int64 `json:"total_hits"`
	Completed    int64 `json:"completed"`
	Resets       int64 `json:"resets"`
}

// ChainMetric contains per-chain execution metrics.
type ChainMetric struct {
	Hits      int64 `json:"hits"`
	Completed int64 `json:"completed"`
	Resets    int64 `json:"resets"`
}

// clusterMetricsHandler returns the current node's metrics as JSON.
// GET /cluster/metrics
//
// Response format:
//
//	{
//	  "timestamp": "2025-01-15T10:30:00Z",
//	  "processing_stats": {
//	    "lines_processed": 1000,
//	    "valid_hits": 42,
//	    ...
//	  },
//	  ...
//	}
func clusterMetricsHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get metrics snapshot from provider
		snapshot := p.GetMetricsSnapshot()

		// Set content type
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		// Encode and send response
		if err := json.NewEncoder(w).Encode(snapshot); err != nil {
			p.Log(3, "CLUSTER", "Failed to encode metrics response: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	}
}
