// Package server provides an HTTP API for accessing bot-detector metrics,
// configuration, and operational data. The server is optional and enabled
// via the --listen flag.
//
// Current endpoints:
//   - GET /                - Plain-text metrics dashboard
//   - GET /stats           - Plain-text metrics dashboard (alias)
//   - GET /stats/steps     - Plain-text step execution counts
//   - GET /config          - Raw YAML configuration
//   - GET /config/archive  - Tar.gz archive of config + dependencies
//   - GET /ip/{ip}         - IP block/unblock status (local node, plain text)
//   - GET /api/v1/ip/{ip}  - IP block/unblock status (local node, JSON)
//   - GET /cluster/status  - Node cluster status (role, name, address, leader)
//   - GET /cluster/metrics - Node metrics snapshot in JSON format
//   - GET /cluster/metrics/aggregate - Cluster-wide aggregated metrics (leader only)
//   - GET /cluster/ip/{ip} - IP status aggregated across cluster (leader only, plain text)
//   - GET /api/v1/cluster/ip/{ip} - IP status aggregated across cluster (leader only, JSON)
//   - GET /api/v1/cluster/internal/ip/{ip} - Internal endpoint for leader to query followers
package server

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"bot-detector/internal/logging"
)

// Start initializes and starts the server(s) in separate goroutines.
// It registers all endpoints with role-based filtering and handles
// graceful shutdown when the application receives a termination signal.
//
// If no listeners are configured, this function returns immediately.
func Start(p Provider) {
	configsInterface := p.GetListenConfigs()
	if configsInterface == nil {
		p.Log(logging.LevelInfo, "SERVER", "Server is disabled.")
		return
	}

	// Convert to slice of our interface type using reflection
	listenConfigs := convertToListenConfigSlice(configsInterface)
	if len(listenConfigs) == 0 {
		p.Log(logging.LevelInfo, "SERVER", "Server is disabled.")
		return
	}

	var wg sync.WaitGroup
	servers := make([]*http.Server, 0, len(listenConfigs))

	// Start a server for each listen config
	for _, cfg := range listenConfigs {
		mux := createRoleFilteredHandler(p, listenConfigs, cfg)

		server := &http.Server{
			Addr:    cfg.GetAddress(),
			Handler: mux,
		}
		servers = append(servers, server)

		wg.Add(1)
		go func(srv *http.Server, config ListenConfig) {
			defer wg.Done()
			logMsg := fmt.Sprintf("Starting server on %s://%s", config.GetProtocol(), config.String())
			if config.HasExplicitRoles() {
				logMsg += fmt.Sprintf(" (roles: %s)", getRolesString(config))
			}
			p.Log(logging.LevelInfo, "SERVER", logMsg)
			if err := srv.ListenAndServe(); err != http.ErrServerClosed {
				p.Log(logging.LevelError, "SERVER", "Server failed on %s: %v", config.GetAddress(), err)
			}
		}(server, cfg)
	}

	// Wait for shutdown signal
	<-p.GetShutdownChannel()
	p.Log(logging.LevelInfo, "SERVER", "Shutting down server(s).")

	// Close all servers
	for _, srv := range servers {
		if err := srv.Close(); err != nil {
			logging.LogOutput(logging.LevelError, "SERVER", "Error stopping server on %s: %v", srv.Addr, err)
		}
	}

	// Wait for all goroutines to finish
	wg.Wait()
}

// convertToListenConfigSlice converts interface{} to []ListenConfig using reflection.
func convertToListenConfigSlice(v interface{}) []ListenConfig {
	if v == nil {
		return nil
	}

	val := reflect.ValueOf(v)
	if val.Kind() != reflect.Slice {
		return nil
	}

	result := make([]ListenConfig, 0, val.Len())
	for i := 0; i < val.Len(); i++ {
		item := val.Index(i).Interface()
		if cfg, ok := item.(ListenConfig); ok {
			result = append(result, cfg)
		}
	}

	return result
}

// createRoleFilteredHandler creates an HTTP handler with role-based endpoint filtering.
func createRoleFilteredHandler(p Provider, allConfigs []ListenConfig, currentConfig ListenConfig) http.Handler {
	mux := http.NewServeMux()

	// Metrics endpoints (role=metrics)
	if shouldServeEndpoint(allConfigs, currentConfig, RoleMetrics) {
		mux.HandleFunc("/", rootHandler(p))
		mux.HandleFunc("/stats", rootHandler(p))
		mux.HandleFunc("/stats/steps", stepsHandler(p))
	}

	// API endpoints (role=api)
	if shouldServeEndpoint(allConfigs, currentConfig, RoleAPI) {
		mux.HandleFunc("/config", configHandler(p))
		mux.HandleFunc("/config/archive", archiveHandler(p))
		mux.HandleFunc("GET /ip/{ip}", ipLookupHandler(p))
		mux.HandleFunc("DELETE /ip/{ip}/clear", unblockIPHandler(p))
		mux.HandleFunc("GET /api/v1/ip/{ip}", apiIPLookupHandler(p))
	}

	// Cluster endpoints (role=cluster)
	if shouldServeEndpoint(allConfigs, currentConfig, RoleCluster) {
		mux.HandleFunc("/cluster/status", clusterStatusHandler(p))
		mux.HandleFunc("/cluster/metrics", clusterMetricsHandler(p))
		mux.HandleFunc("/cluster/metrics/aggregate", clusterMetricsAggregateHandler(p))
		mux.HandleFunc("GET /cluster/ip/{ip}", clusterIPAggregateHandler(p))
		mux.HandleFunc("GET /api/v1/cluster/ip/{ip}", apiClusterIPAggregateHandler(p))
		mux.HandleFunc("GET /api/v1/cluster/internal/ip/{ip}", clusterIPLookupHandler(p))
		mux.HandleFunc("DELETE /api/v1/cluster/internal/ip/{ip}/clear", internalClearIPHandler(p))
	}

	// Wrap with logging middleware
	return loggingMiddleware(p, mux)
}

// loggingMiddleware logs incoming HTTP requests in debug mode.
func loggingMiddleware(p Provider, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.Log(logging.LevelDebug, "SERVER", "Request: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

// getRolesString extracts role information from a ListenConfig for logging.
func getRolesString(config ListenConfig) string {
	// Extract roles by checking which ones the config has
	var roles []string
	for _, role := range []string{"api", "metrics", "cluster", "all"} {
		if config.HasRole(role) {
			roles = append(roles, role)
		}
	}
	if len(roles) == 0 {
		return "all"
	}
	return strings.Join(roles, ", ")
}
