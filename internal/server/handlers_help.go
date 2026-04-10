package server

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type endpoint struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description"`
	ContentType string `json:"content_type"`
	Role        string `json:"role"`
}

var allEndpoints = []endpoint{
	// Help
	{"GET", "/help", "List all endpoints (plain text)", "text/plain", "all"},
	{"GET", "/api/v1/help", "List API endpoints (JSON)", "application/json", "api"},

	// Metrics
	{"GET", "/", "Metrics dashboard", "text/plain", "metrics"},
	{"GET", "/stats", "Metrics dashboard (alias)", "text/plain", "metrics"},
	{"GET", "/stats/steps", "Step execution counts", "text/plain", "metrics"},
	{"GET", "/stats/websites", "Multi-website statistics", "text/plain", "metrics"},

	// API
	{"GET", "/config", "Raw YAML configuration", "text/yaml", "api"},
	{"GET", "/config/archive", "Tar.gz archive of config + dependencies", "application/gzip", "api"},
	{"GET", "/ip/{ip}", "IP block status (cluster-aware)", "text/plain", "api"},
	{"DELETE", "/ip/{ip}/clear", "Clear IP from all state (cluster-aware)", "text/plain", "api"},
	{"GET|POST", "/ip/{ip}/unblock", "Fast unblock IP (cluster-aware)", "text/plain", "api"},
	{"GET", "/api/v1/ip/{ip}", "IP block status (cluster-aware)", "application/json", "api"},
	{"GET", "/api/v1/bad-actors", "List all bad actors", "application/json", "api"},
	{"GET", "/api/v1/bad-actors/export", "Bad actor IPs, one per line", "text/plain", "api"},
	{"GET", "/api/v1/bad-actors/stats", "Bad actor statistics", "application/json", "api"},
	{"DELETE", "/api/v1/bad-actors?reason=<reason>[&unblock]", "Remove bad actors by reason", "application/json", "api"},

	// Cluster
	{"GET", "/cluster/status", "Node cluster status", "application/json", "cluster"},
	{"GET", "/cluster/metrics", "Node metrics snapshot", "application/json", "cluster"},
	{"GET", "/cluster/metrics/aggregate", "Cluster-wide aggregated metrics (leader only)", "application/json", "cluster"},
	{"GET", "/api/v1/cluster/internal/ip/{ip}", "Internal: query follower IP status", "application/json", "cluster"},
	{"DELETE", "/api/v1/cluster/internal/ip/{ip}/clear", "Internal: broadcast clear to follower", "text/plain", "cluster"},
	{"GET|POST", "/api/v1/cluster/internal/ip/{ip}/unblock", "Internal: broadcast unblock to follower", "text/plain", "cluster"},
	{"GET", "/api/v1/cluster/internal/persistence/state", "Internal: persistence state for sync", "application/json", "cluster"},
	{"GET", "/api/v1/cluster/state/merged", "Merged cluster state", "application/json", "cluster"},
}

// helpHandler returns endpoint listing.
// filter selects which endpoints to include (empty string = all).
// If asJSON is true, returns JSON; otherwise plain text.
func helpHandler(filter string, asJSON bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var result []endpoint
		for _, e := range allEndpoints {
			if filter == "" || e.Role == filter {
				result = append(result, e)
			}
		}

		if asJSON {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(result) //nolint:errcheck
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, e := range result {
			fmt.Fprintf(w, "%-10s %-55s %-18s %s\n", e.Method, e.Path, e.ContentType, e.Description) //nolint:errcheck
		}
	}
}
