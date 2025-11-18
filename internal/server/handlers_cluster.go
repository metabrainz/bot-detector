package server

import (
	"encoding/json"
	"net/http"
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
