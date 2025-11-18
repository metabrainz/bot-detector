package cluster

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/server"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// mockLogFunc is a simple log function for testing.
func mockLogFunc(level logging.LogLevel, tag string, format string, v ...interface{}) {
	// Intentionally empty for tests
}

func TestNewMetricsCollector(t *testing.T) {
	shutdownCh := make(chan os.Signal, 1)
	nodes := []NodeConfig{
		{Name: "node1", Address: "localhost:8080"},
		{Name: "node2", Address: "localhost:9090"},
	}

	collector := NewMetricsCollector(MetricsCollectorOptions{
		Nodes:        nodes,
		PollInterval: 10 * time.Second,
		ShutdownCh:   shutdownCh,
		LogFunc:      mockLogFunc,
		HTTPTimeout:  5 * time.Second,
		Protocol:     "http",
	})

	if collector == nil {
		t.Fatal("Expected collector to be created")
	}

	if len(collector.nodes) != 2 {
		t.Errorf("Expected 2 nodes, got %d", len(collector.nodes))
	}

	if collector.pollInterval != 10*time.Second {
		t.Errorf("Expected poll interval 10s, got %v", collector.pollInterval)
	}

	if collector.protocol != "http" {
		t.Errorf("Expected protocol 'http', got %s", collector.protocol)
	}
}

func TestMetricsCollector_SuccessfulCollection(t *testing.T) {
	// Create a mock metrics response
	mockMetrics := server.MetricsSnapshot{
		Timestamp: time.Now(),
		ProcessingStats: server.ProcessingStats{
			LinesProcessed: 100,
			ValidHits:      50,
		},
		ActorStats: server.ActorStats{
			GoodActorsSkipped: 5,
		},
		ChainStats: server.ChainStats{
			ActionsBlock: 10,
			ActionsLog:   20,
		},
	}

	// Create a test HTTP server that returns mock metrics
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cluster/metrics" {
			t.Errorf("Expected path '/cluster/metrics', got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(mockMetrics)
	}))
	defer server.Close()

	// Extract just the address part (remove http://)
	address := server.URL[7:] // Remove "http://"

	shutdownCh := make(chan os.Signal, 1)
	collector := NewMetricsCollector(MetricsCollectorOptions{
		Nodes: []NodeConfig{
			{Name: "test-node", Address: address},
		},
		PollInterval: 100 * time.Millisecond,
		ShutdownCh:   shutdownCh,
		LogFunc:      mockLogFunc,
		HTTPTimeout:  2 * time.Second,
		Protocol:     "http",
	})

	// Collect metrics once
	collector.collectFromAllNodes()

	// Check that metrics were collected
	metrics := collector.GetCollectedMetrics()
	if len(metrics) != 1 {
		t.Fatalf("Expected 1 node metrics, got %d", len(metrics))
	}

	nodeMetrics, exists := metrics["test-node"]
	if !exists {
		t.Fatal("Expected metrics for 'test-node'")
	}

	if nodeMetrics.Snapshot == nil {
		t.Fatal("Expected snapshot to be populated")
	}

	if nodeMetrics.Snapshot.ProcessingStats.LinesProcessed != 100 {
		t.Errorf("Expected 100 lines processed, got %d", nodeMetrics.Snapshot.ProcessingStats.LinesProcessed)
	}

	if nodeMetrics.ConsecutiveErrors != 0 {
		t.Errorf("Expected 0 consecutive errors, got %d", nodeMetrics.ConsecutiveErrors)
	}

	if nodeMetrics.LastError != "" {
		t.Errorf("Expected no error, got %s", nodeMetrics.LastError)
	}
}

func TestMetricsCollector_HTTPError(t *testing.T) {
	// Create a test HTTP server that returns an error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	address := server.URL[7:]

	shutdownCh := make(chan os.Signal, 1)
	collector := NewMetricsCollector(MetricsCollectorOptions{
		Nodes: []NodeConfig{
			{Name: "error-node", Address: address},
		},
		PollInterval: 100 * time.Millisecond,
		ShutdownCh:   shutdownCh,
		LogFunc:      mockLogFunc,
		HTTPTimeout:  2 * time.Second,
		Protocol:     "http",
	})

	// Collect metrics once
	collector.collectFromAllNodes()

	// Check that error was recorded
	metrics := collector.GetCollectedMetrics()
	if len(metrics) != 1 {
		t.Fatalf("Expected 1 node metrics, got %d", len(metrics))
	}

	nodeMetrics, exists := metrics["error-node"]
	if !exists {
		t.Fatal("Expected metrics for 'error-node'")
	}

	if nodeMetrics.ConsecutiveErrors != 1 {
		t.Errorf("Expected 1 consecutive error, got %d", nodeMetrics.ConsecutiveErrors)
	}

	if nodeMetrics.LastError == "" {
		t.Error("Expected error to be recorded")
	}
}

func TestMetricsCollector_MalformedJSON(t *testing.T) {
	// Create a test HTTP server that returns invalid JSON
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{invalid json}"))
	}))
	defer server.Close()

	address := server.URL[7:]

	shutdownCh := make(chan os.Signal, 1)
	collector := NewMetricsCollector(MetricsCollectorOptions{
		Nodes: []NodeConfig{
			{Name: "malformed-node", Address: address},
		},
		PollInterval: 100 * time.Millisecond,
		ShutdownCh:   shutdownCh,
		LogFunc:      mockLogFunc,
		HTTPTimeout:  2 * time.Second,
		Protocol:     "http",
	})

	// Collect metrics once
	collector.collectFromAllNodes()

	// Check that error was recorded
	metrics := collector.GetCollectedMetrics()
	nodeMetrics := metrics["malformed-node"]

	if nodeMetrics.ConsecutiveErrors != 1 {
		t.Errorf("Expected 1 consecutive error, got %d", nodeMetrics.ConsecutiveErrors)
	}

	if nodeMetrics.LastError == "" {
		t.Error("Expected JSON parse error to be recorded")
	}
}

func TestMetricsCollector_UnreachableNode(t *testing.T) {
	shutdownCh := make(chan os.Signal, 1)
	collector := NewMetricsCollector(MetricsCollectorOptions{
		Nodes: []NodeConfig{
			{Name: "unreachable", Address: "localhost:99999"}, // Invalid port
		},
		PollInterval: 100 * time.Millisecond,
		ShutdownCh:   shutdownCh,
		LogFunc:      mockLogFunc,
		HTTPTimeout:  1 * time.Second,
		Protocol:     "http",
	})

	// Collect metrics once
	collector.collectFromAllNodes()

	// Check that error was recorded
	metrics := collector.GetCollectedMetrics()
	nodeMetrics := metrics["unreachable"]

	if nodeMetrics.ConsecutiveErrors != 1 {
		t.Errorf("Expected 1 consecutive error, got %d", nodeMetrics.ConsecutiveErrors)
	}

	if nodeMetrics.LastError == "" {
		t.Error("Expected connection error to be recorded")
	}
}

func TestMetricsCollector_ConsecutiveErrors(t *testing.T) {
	shutdownCh := make(chan os.Signal, 1)
	collector := NewMetricsCollector(MetricsCollectorOptions{
		Nodes: []NodeConfig{
			{Name: "failing-node", Address: "localhost:99998"},
		},
		PollInterval: 100 * time.Millisecond,
		ShutdownCh:   shutdownCh,
		LogFunc:      mockLogFunc,
		HTTPTimeout:  1 * time.Second,
		Protocol:     "http",
	})

	// Collect multiple times to accumulate errors
	for i := 0; i < 3; i++ {
		collector.collectFromAllNodes()
	}

	// Check that consecutive errors were tracked
	metrics := collector.GetCollectedMetrics()
	nodeMetrics := metrics["failing-node"]

	if nodeMetrics.ConsecutiveErrors != 3 {
		t.Errorf("Expected 3 consecutive errors, got %d", nodeMetrics.ConsecutiveErrors)
	}
}

func TestMetricsCollector_RecoveryFromError(t *testing.T) {
	failCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCount++
		if failCount <= 2 {
			// Fail first 2 times
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Succeed on 3rd attempt
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		mockMetrics := server.MetricsSnapshot{Timestamp: time.Now()}
		_ = json.NewEncoder(w).Encode(mockMetrics)
	}))
	defer server.Close()

	address := server.URL[7:]

	shutdownCh := make(chan os.Signal, 1)
	collector := NewMetricsCollector(MetricsCollectorOptions{
		Nodes: []NodeConfig{
			{Name: "recovery-node", Address: address},
		},
		PollInterval: 100 * time.Millisecond,
		ShutdownCh:   shutdownCh,
		LogFunc:      mockLogFunc,
		HTTPTimeout:  2 * time.Second,
		Protocol:     "http",
	})

	// Collect 3 times
	collector.collectFromAllNodes()
	collector.collectFromAllNodes()
	collector.collectFromAllNodes()

	// Check that error was cleared on recovery
	metrics := collector.GetCollectedMetrics()
	nodeMetrics := metrics["recovery-node"]

	if nodeMetrics.ConsecutiveErrors != 0 {
		t.Errorf("Expected 0 consecutive errors after recovery, got %d", nodeMetrics.ConsecutiveErrors)
	}

	if nodeMetrics.LastError != "" {
		t.Errorf("Expected error to be cleared after recovery, got %s", nodeMetrics.LastError)
	}

	if nodeMetrics.Snapshot == nil {
		t.Error("Expected snapshot to be populated after recovery")
	}
}

func TestMetricsCollector_GetCollectedMetrics_ThreadSafety(t *testing.T) {
	shutdownCh := make(chan os.Signal, 1)
	collector := NewMetricsCollector(MetricsCollectorOptions{
		Nodes: []NodeConfig{
			{Name: "node1", Address: "localhost:8080"},
		},
		PollInterval: 100 * time.Millisecond,
		ShutdownCh:   shutdownCh,
		LogFunc:      mockLogFunc,
		HTTPTimeout:  1 * time.Second,
		Protocol:     "http",
	})

	// Simulate concurrent access
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				_ = collector.GetCollectedMetrics()
			}
			done <- true
		}()
	}

	go func() {
		for j := 0; j < 100; j++ {
			collector.collectFromAllNodes()
			time.Sleep(time.Millisecond)
		}
	}()

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// If we get here without a race condition, the test passes
}

func TestMetricsCollector_DefaultHTTPTimeout(t *testing.T) {
	shutdownCh := make(chan os.Signal, 1)
	collector := NewMetricsCollector(MetricsCollectorOptions{
		Nodes: []NodeConfig{
			{Name: "node1", Address: "localhost:8080"},
		},
		PollInterval: 10 * time.Second,
		ShutdownCh:   shutdownCh,
		LogFunc:      mockLogFunc,
		// HTTPTimeout not set - should default to 10s
		Protocol: "http",
	})

	if collector.httpClient.Timeout != 10*time.Second {
		t.Errorf("Expected default timeout 10s, got %v", collector.httpClient.Timeout)
	}
}

func TestMetricsCollector_DefaultProtocol(t *testing.T) {
	shutdownCh := make(chan os.Signal, 1)
	collector := NewMetricsCollector(MetricsCollectorOptions{
		Nodes: []NodeConfig{
			{Name: "node1", Address: "localhost:8080"},
		},
		PollInterval: 10 * time.Second,
		ShutdownCh:   shutdownCh,
		LogFunc:      mockLogFunc,
		// Protocol not set - should default to "http"
	})

	if collector.protocol != "http" {
		t.Errorf("Expected default protocol 'http', got %s", collector.protocol)
	}
}
