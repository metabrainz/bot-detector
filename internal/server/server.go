// Package server provides an HTTP API for accessing bot-detector metrics,
// configuration, and operational data. The server is optional and enabled
// via the --http-server flag.
//
// Current endpoints:
//   - GET /               - HTML metrics dashboard
//   - GET /stats          - HTML metrics dashboard (alias)
//   - GET /stats/steps    - Plain-text step execution counts
//   - GET /config         - Raw YAML configuration
//   - GET /config/archive - Tar.gz archive of config + dependencies
//   - GET /cluster/status - Node cluster status (role, name, address, leader)
//   - GET /cluster/metrics - Node metrics snapshot in JSON format
//   - GET /cluster/metrics/aggregate - Cluster-wide aggregated metrics (leader only)
package server

import (
	"net/http"

	"bot-detector/internal/logging"
)

// Start initializes and starts the HTTP server in a separate goroutine.
// It registers all HTTP endpoints and handles graceful shutdown when the
// application receives a termination signal.
//
// If the HTTP server is disabled (empty listen address), this function
// returns immediately without starting the server.
func Start(p Provider) {
	listenAddr := p.GetListenAddr()
	if listenAddr == "" {
		p.Log(logging.LevelInfo, "HTTP_SERVER", "HTTP server is disabled.")
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", rootHandler(p))
	mux.HandleFunc("/stats", rootHandler(p)) // Alias for root
	mux.HandleFunc("/stats/steps", stepsHandler(p))
	mux.HandleFunc("/config", configHandler(p))
	mux.HandleFunc("/config/archive", archiveHandler(p))
	mux.HandleFunc("/cluster/status", clusterStatusHandler(p))
	mux.HandleFunc("/cluster/metrics", clusterMetricsHandler(p))
	mux.HandleFunc("/cluster/metrics/aggregate", clusterMetricsAggregateHandler(p))

	server := &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}

	go func() {
		p.Log(logging.LevelInfo, "HTTP_SERVER", "Starting web server on http://%s", listenAddr)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			p.Log(logging.LevelError, "HTTP_SERVER", "Web server failed: %v", err)
		}
	}()

	// Wait for shutdown signal
	<-p.GetShutdownChannel()
	p.Log(logging.LevelInfo, "HTTP_SERVER", "Shutting down web server.")
	if err := server.Close(); err != nil {
		logging.LogOutput(logging.LevelError, "StopServer", "Error stopping server: %v", err)
	}
}
