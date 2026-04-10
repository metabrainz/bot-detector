package cluster

import (
	"bot-detector/internal/logging"
	"bot-detector/internal/server"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// CollectedMetrics stores metrics collected from a follower node.
type CollectedMetrics struct {
	NodeName          string                  `json:"node_name"`
	Snapshot          *server.MetricsSnapshot `json:"snapshot"`
	LastCollected     time.Time               `json:"last_collected"`
	LastError         string                  `json:"last_error,omitempty"`
	ConsecutiveErrors int                     `json:"consecutive_errors"`
}

// MetricsCollector periodically polls follower nodes for metrics.
type MetricsCollector struct {
	nodes        []NodeConfig
	pollInterval time.Duration
	httpClient   *http.Client
	metrics      map[string]*CollectedMetrics
	metricsMutex sync.RWMutex
	shutdownCh   <-chan os.Signal
	logFunc      LogFunc
	protocol     string
}

// MetricsCollectorOptions contains configuration for the metrics collector.
type MetricsCollectorOptions struct {
	Nodes        []NodeConfig     // List of nodes to collect metrics from
	PollInterval time.Duration    // How often to poll for metrics
	ShutdownCh   <-chan os.Signal // Shutdown signal channel
	LogFunc      LogFunc          // Logging function
	HTTPTimeout  time.Duration    // HTTP request timeout
	Protocol     string           // Protocol to use (http or https)
}

// NewMetricsCollector creates a new metrics collector for leader nodes.
func NewMetricsCollector(opts MetricsCollectorOptions) *MetricsCollector {
	if opts.HTTPTimeout == 0 {
		opts.HTTPTimeout = 10 * time.Second
	}

	if opts.Protocol == "" {
		opts.Protocol = "http"
	}

	return &MetricsCollector{
		nodes:        opts.Nodes,
		pollInterval: opts.PollInterval,
		httpClient: &http.Client{
			Timeout: opts.HTTPTimeout,
		},
		metrics:    make(map[string]*CollectedMetrics),
		shutdownCh: opts.ShutdownCh,
		logFunc:    opts.LogFunc,
		protocol:   opts.Protocol,
	}
}

// Start begins polling follower nodes for metrics.
// This runs in a goroutine and should only be called once.
func (mc *MetricsCollector) Start() {
	mc.logFunc(logging.LevelInfo, "CLUSTER", "Starting metrics collector (interval: %s)", mc.pollInterval)

	ticker := time.NewTicker(mc.pollInterval)
	defer ticker.Stop()

	// Collect metrics immediately on start
	mc.collectFromAllNodes()

	for {
		select {
		case <-ticker.C:
			mc.collectFromAllNodes()
		case <-mc.shutdownCh:
			mc.logFunc(logging.LevelInfo, "CLUSTER", "Metrics collector shutting down")
			return
		}
	}
}

// collectFromAllNodes polls all configured nodes for their metrics.
func (mc *MetricsCollector) collectFromAllNodes() {
	for _, node := range mc.nodes {
		mc.collectFromNode(node)
	}
}

// collectFromNode polls a single node for its metrics.
func (mc *MetricsCollector) collectFromNode(node NodeConfig) {
	// Build metrics URL
	metricsURL := fmt.Sprintf("%s://%s/api/v1/cluster/metrics", mc.protocol, node.Address)

	// Make HTTP request
	resp, err := mc.httpClient.Get(metricsURL)
	if err != nil {
		mc.handleCollectionError(node.Name, fmt.Sprintf("HTTP request failed: %v", err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		mc.handleCollectionError(node.Name, fmt.Sprintf("HTTP %d", resp.StatusCode))
		return
	}

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		mc.handleCollectionError(node.Name, fmt.Sprintf("Failed to read response: %v", err))
		return
	}

	// Parse JSON
	var snapshot server.MetricsSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		mc.handleCollectionError(node.Name, fmt.Sprintf("Failed to parse JSON: %v", err))
		return
	}

	// Store metrics
	mc.metricsMutex.Lock()
	mc.metrics[node.Name] = &CollectedMetrics{
		NodeName:          node.Name,
		Snapshot:          &snapshot,
		LastCollected:     time.Now(),
		LastError:         "",
		ConsecutiveErrors: 0,
	}
	mc.metricsMutex.Unlock()

	mc.logFunc(logging.LevelDebug, "CLUSTER", "Collected metrics from node '%s'", node.Name)
}

// handleCollectionError records a collection error for a node.
func (mc *MetricsCollector) handleCollectionError(nodeName, errorMsg string) {
	mc.metricsMutex.Lock()
	defer mc.metricsMutex.Unlock()

	// Get or create metrics entry
	collected, exists := mc.metrics[nodeName]
	if !exists {
		collected = &CollectedMetrics{
			NodeName: nodeName,
		}
		mc.metrics[nodeName] = collected
	}

	// Update error info
	collected.LastError = errorMsg
	collected.ConsecutiveErrors++

	// Log at different levels based on consecutive failures
	logLevel := logging.LevelWarning
	if collected.ConsecutiveErrors >= 5 {
		logLevel = logging.LevelError
	}

	mc.logFunc(logLevel, "CLUSTER", "Failed to collect metrics from node '%s' (%d consecutive failures): %s",
		nodeName, collected.ConsecutiveErrors, errorMsg)
}

// GetCollectedMetrics returns a thread-safe copy of collected metrics.
func (mc *MetricsCollector) GetCollectedMetrics() map[string]*CollectedMetrics {
	mc.metricsMutex.RLock()
	defer mc.metricsMutex.RUnlock()

	// Create a copy to avoid race conditions
	result := make(map[string]*CollectedMetrics, len(mc.metrics))
	for name, metrics := range mc.metrics {
		// Create a shallow copy (snapshot pointer is shared, but that's intentional)
		metricsCopy := *metrics
		result[name] = &metricsCopy
	}

	return result
}
