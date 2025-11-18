package cluster

import (
	"bot-detector/internal/server"
	"testing"
	"time"
)

func TestDetermineNodeHealth(t *testing.T) {
	tests := []struct {
		name           string
		collected      *CollectedMetrics
		staleThreshold time.Duration
		expected       NodeHealthStatus
	}{
		{
			name: "Healthy node",
			collected: &CollectedMetrics{
				NodeName:          "node1",
				Snapshot:          &server.MetricsSnapshot{},
				LastCollected:     time.Now().Add(-1 * time.Second),
				LastError:         "",
				ConsecutiveErrors: 0,
			},
			staleThreshold: 5 * time.Second,
			expected:       NodeHealthy,
		},
		{
			name: "Stale node",
			collected: &CollectedMetrics{
				NodeName:          "node2",
				Snapshot:          &server.MetricsSnapshot{},
				LastCollected:     time.Now().Add(-10 * time.Second),
				LastError:         "",
				ConsecutiveErrors: 0,
			},
			staleThreshold: 5 * time.Second,
			expected:       NodeStale,
		},
		{
			name: "Error node - consecutive errors",
			collected: &CollectedMetrics{
				NodeName:          "node3",
				Snapshot:          nil,
				LastCollected:     time.Now(),
				LastError:         "connection refused",
				ConsecutiveErrors: 3,
			},
			staleThreshold: 5 * time.Second,
			expected:       NodeError,
		},
		{
			name: "Error node - no snapshot",
			collected: &CollectedMetrics{
				NodeName:          "node4",
				Snapshot:          nil,
				LastCollected:     time.Now(),
				LastError:         "",
				ConsecutiveErrors: 0,
			},
			staleThreshold: 5 * time.Second,
			expected:       NodeError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := determineNodeHealth(tt.collected, tt.staleThreshold)
			if result != tt.expected {
				t.Errorf("Expected status %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestSumInt64Maps(t *testing.T) {
	dst := map[string]int64{
		"key1": 10,
		"key2": 20,
	}
	src := map[string]int64{
		"key2": 5,
		"key3": 15,
	}

	sumInt64Maps(dst, src)

	expected := map[string]int64{
		"key1": 10,
		"key2": 25,
		"key3": 15,
	}

	for key, expectedVal := range expected {
		if dst[key] != expectedVal {
			t.Errorf("For key %s: expected %d, got %d", key, expectedVal, dst[key])
		}
	}
}

func TestSumProcessingStats(t *testing.T) {
	dst := &server.ProcessingStats{
		LinesProcessed: 100,
		ValidHits:      50,
		ParseErrors:    5,
		ReorderedLines: 10,
		TimeElapsed:    10.0,
	}
	src := &server.ProcessingStats{
		LinesProcessed: 200,
		ValidHits:      75,
		ParseErrors:    3,
		ReorderedLines: 15,
		TimeElapsed:    20.0,
	}

	sumProcessingStats(dst, src)

	if dst.LinesProcessed != 300 {
		t.Errorf("Expected LinesProcessed=300, got %d", dst.LinesProcessed)
	}
	if dst.ValidHits != 125 {
		t.Errorf("Expected ValidHits=125, got %d", dst.ValidHits)
	}
	if dst.ParseErrors != 8 {
		t.Errorf("Expected ParseErrors=8, got %d", dst.ParseErrors)
	}
	if dst.ReorderedLines != 25 {
		t.Errorf("Expected ReorderedLines=25, got %d", dst.ReorderedLines)
	}
	if dst.TimeElapsed != 30.0 {
		t.Errorf("Expected TimeElapsed=30.0, got %f", dst.TimeElapsed)
	}
}

func TestSumActorStats(t *testing.T) {
	dst := &server.ActorStats{
		GoodActorsSkipped: 10,
		ActorsCleaned:     5,
	}
	src := &server.ActorStats{
		GoodActorsSkipped: 15,
		ActorsCleaned:     3,
	}

	sumActorStats(dst, src)

	if dst.GoodActorsSkipped != 25 {
		t.Errorf("Expected GoodActorsSkipped=25, got %d", dst.GoodActorsSkipped)
	}
	if dst.ActorsCleaned != 8 {
		t.Errorf("Expected ActorsCleaned=8, got %d", dst.ActorsCleaned)
	}
}

func TestSumChainStats(t *testing.T) {
	dst := &server.ChainStats{
		ActionsBlock: 10,
		ActionsLog:   20,
		TotalHits:    100,
		Completed:    50,
		Resets:       5,
	}
	src := &server.ChainStats{
		ActionsBlock: 5,
		ActionsLog:   10,
		TotalHits:    200,
		Completed:    75,
		Resets:       3,
	}

	sumChainStats(dst, src)

	if dst.ActionsBlock != 15 {
		t.Errorf("Expected ActionsBlock=15, got %d", dst.ActionsBlock)
	}
	if dst.ActionsLog != 30 {
		t.Errorf("Expected ActionsLog=30, got %d", dst.ActionsLog)
	}
	if dst.TotalHits != 300 {
		t.Errorf("Expected TotalHits=300, got %d", dst.TotalHits)
	}
	if dst.Completed != 125 {
		t.Errorf("Expected Completed=125, got %d", dst.Completed)
	}
	if dst.Resets != 8 {
		t.Errorf("Expected Resets=8, got %d", dst.Resets)
	}
}

func TestSumChainMetrics(t *testing.T) {
	dst := map[string]server.ChainMetric{
		"chain1": {Hits: 10, Completed: 5, Resets: 1},
		"chain2": {Hits: 20, Completed: 10, Resets: 2},
	}
	src := map[string]server.ChainMetric{
		"chain2": {Hits: 5, Completed: 3, Resets: 1},
		"chain3": {Hits: 15, Completed: 8, Resets: 0},
	}

	sumChainMetrics(dst, src)

	// chain1 should be unchanged
	if dst["chain1"].Hits != 10 || dst["chain1"].Completed != 5 || dst["chain1"].Resets != 1 {
		t.Errorf("chain1 should be unchanged")
	}
	// chain2 should be summed
	if dst["chain2"].Hits != 25 || dst["chain2"].Completed != 13 || dst["chain2"].Resets != 3 {
		t.Errorf("chain2 should be summed: got %+v", dst["chain2"])
	}
	// chain3 should be added
	if dst["chain3"].Hits != 15 || dst["chain3"].Completed != 8 || dst["chain3"].Resets != 0 {
		t.Errorf("chain3 should be added: got %+v", dst["chain3"])
	}
}

func TestAggregateMetrics_AllHealthy(t *testing.T) {
	now := time.Now()

	nodes := []NodeConfig{
		{Name: "node1", Address: "localhost:9090"},
		{Name: "node2", Address: "localhost:9091"},
	}

	collected := map[string]*CollectedMetrics{
		"node1": {
			NodeName:          "node1",
			LastCollected:     now.Add(-1 * time.Second),
			ConsecutiveErrors: 0,
			LastError:         "",
			Snapshot: &server.MetricsSnapshot{
				Timestamp: now,
				ProcessingStats: server.ProcessingStats{
					LinesProcessed: 100,
					ValidHits:      50,
					TimeElapsed:    10.0,
				},
				ActorStats: server.ActorStats{
					GoodActorsSkipped: 10,
					ActorsCleaned:     5,
				},
				ChainStats: server.ChainStats{
					ActionsBlock: 5,
					ActionsLog:   10,
				},
				GoodActorHits: map[string]int64{"actor1": 10},
			},
		},
		"node2": {
			NodeName:          "node2",
			LastCollected:     now.Add(-2 * time.Second),
			ConsecutiveErrors: 0,
			LastError:         "",
			Snapshot: &server.MetricsSnapshot{
				Timestamp: now,
				ProcessingStats: server.ProcessingStats{
					LinesProcessed: 200,
					ValidHits:      75,
					TimeElapsed:    20.0,
				},
				ActorStats: server.ActorStats{
					GoodActorsSkipped: 15,
					ActorsCleaned:     3,
				},
				ChainStats: server.ChainStats{
					ActionsBlock: 10,
					ActionsLog:   5,
				},
				GoodActorHits: map[string]int64{"actor1": 5, "actor2": 15},
			},
		},
	}

	staleThreshold := 10 * time.Second
	result := AggregateMetrics(collected, nodes, staleThreshold)

	// Check node counts
	if result.TotalNodes != 2 {
		t.Errorf("Expected TotalNodes=2, got %d", result.TotalNodes)
	}
	if result.HealthyNodes != 2 {
		t.Errorf("Expected HealthyNodes=2, got %d", result.HealthyNodes)
	}
	if result.StaleNodes != 0 {
		t.Errorf("Expected StaleNodes=0, got %d", result.StaleNodes)
	}
	if result.ErrorNodes != 0 {
		t.Errorf("Expected ErrorNodes=0, got %d", result.ErrorNodes)
	}

	// Check aggregated stats
	if result.Aggregated.ProcessingStats.LinesProcessed != 300 {
		t.Errorf("Expected aggregated LinesProcessed=300, got %d", result.Aggregated.ProcessingStats.LinesProcessed)
	}
	if result.Aggregated.ProcessingStats.ValidHits != 125 {
		t.Errorf("Expected aggregated ValidHits=125, got %d", result.Aggregated.ProcessingStats.ValidHits)
	}
	if result.Aggregated.ActorStats.GoodActorsSkipped != 25 {
		t.Errorf("Expected aggregated GoodActorsSkipped=25, got %d", result.Aggregated.ActorStats.GoodActorsSkipped)
	}
	if result.Aggregated.ChainStats.ActionsBlock != 15 {
		t.Errorf("Expected aggregated ActionsBlock=15, got %d", result.Aggregated.ChainStats.ActionsBlock)
	}

	// Check lines per second calculation
	expectedLPS := float64(300) / 30.0 // 300 lines / 30 seconds
	if result.Aggregated.ProcessingStats.LinesPerSecond != expectedLPS {
		t.Errorf("Expected LinesPerSecond=%.2f, got %.2f", expectedLPS, result.Aggregated.ProcessingStats.LinesPerSecond)
	}

	// Check aggregated maps
	if result.Aggregated.GoodActorHits["actor1"] != 15 {
		t.Errorf("Expected actor1 hits=15, got %d", result.Aggregated.GoodActorHits["actor1"])
	}
	if result.Aggregated.GoodActorHits["actor2"] != 15 {
		t.Errorf("Expected actor2 hits=15, got %d", result.Aggregated.GoodActorHits["actor2"])
	}

	// Check per-node data
	if len(result.Nodes) != 2 {
		t.Errorf("Expected 2 node entries, got %d", len(result.Nodes))
	}
}

func TestAggregateMetrics_MixedHealth(t *testing.T) {
	now := time.Now()

	nodes := []NodeConfig{
		{Name: "healthy", Address: "localhost:9090"},
		{Name: "stale", Address: "localhost:9091"},
		{Name: "error", Address: "localhost:9092"},
	}

	collected := map[string]*CollectedMetrics{
		"healthy": {
			NodeName:          "healthy",
			LastCollected:     now.Add(-1 * time.Second),
			ConsecutiveErrors: 0,
			Snapshot:          &server.MetricsSnapshot{Timestamp: now},
		},
		"stale": {
			NodeName:          "stale",
			LastCollected:     now.Add(-15 * time.Second), // Stale (> 10 seconds)
			ConsecutiveErrors: 0,
			Snapshot:          &server.MetricsSnapshot{Timestamp: now},
		},
		"error": {
			NodeName:          "error",
			LastCollected:     now,
			ConsecutiveErrors: 3,
			LastError:         "connection failed",
			Snapshot:          nil,
		},
	}

	staleThreshold := 10 * time.Second
	result := AggregateMetrics(collected, nodes, staleThreshold)

	if result.HealthyNodes != 1 {
		t.Errorf("Expected HealthyNodes=1, got %d", result.HealthyNodes)
	}
	if result.StaleNodes != 1 {
		t.Errorf("Expected StaleNodes=1, got %d", result.StaleNodes)
	}
	if result.ErrorNodes != 1 {
		t.Errorf("Expected ErrorNodes=1, got %d", result.ErrorNodes)
	}

	// Verify node statuses in result
	statusCounts := make(map[NodeHealthStatus]int)
	for _, node := range result.Nodes {
		statusCounts[node.Status]++
	}

	if statusCounts[NodeHealthy] != 1 {
		t.Errorf("Expected 1 healthy node in results, got %d", statusCounts[NodeHealthy])
	}
	if statusCounts[NodeStale] != 1 {
		t.Errorf("Expected 1 stale node in results, got %d", statusCounts[NodeStale])
	}
	if statusCounts[NodeError] != 1 {
		t.Errorf("Expected 1 error node in results, got %d", statusCounts[NodeError])
	}
}

func TestAggregateMetrics_EmptySnapshot(t *testing.T) {
	nodes := []NodeConfig{
		{Name: "node1", Address: "localhost:9090"},
	}

	collected := map[string]*CollectedMetrics{
		"node1": {
			NodeName:          "node1",
			LastCollected:     time.Now(),
			ConsecutiveErrors: 0,
			Snapshot:          nil, // No snapshot
		},
	}

	result := AggregateMetrics(collected, nodes, 10*time.Second)

	// Should not panic, and aggregated metrics should be zero
	if result.Aggregated.ProcessingStats.LinesProcessed != 0 {
		t.Errorf("Expected zero lines processed with nil snapshot")
	}

	// Node should be marked as error due to missing snapshot
	if result.ErrorNodes != 1 {
		t.Errorf("Expected ErrorNodes=1 for nil snapshot, got %d", result.ErrorNodes)
	}
}
