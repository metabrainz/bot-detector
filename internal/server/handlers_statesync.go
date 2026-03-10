package server

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"time"

	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
)

const (
	// StateSyncVersion is the current state sync protocol version.
	StateSyncVersion = "v1"
)

// clusterPersistenceStateHandler exposes the node's local IPStates for state sync.
// GET /api/v1/cluster/internal/persistence/state?since=<timestamp>
func clusterPersistenceStateHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse optional "since" parameter for incremental sync
		sinceStr := r.URL.Query().Get("since")
		var since time.Time
		incremental := false
		if sinceStr != "" {
			var err error
			since, err = time.Parse(time.RFC3339, sinceStr)
			if err != nil {
				http.Error(w, "Invalid 'since' timestamp format", http.StatusBadRequest)
				return
			}
			incremental = true
		}

		// Collect local IPStates
		p.GetPersistenceMutex().Lock()
		states := make(map[string]persistence.IPState)
		ipStates := p.GetIPStates()

		for ip, state := range ipStates {
			// Skip expired entries
			if !state.ExpireTime.IsZero() && time.Now().After(state.ExpireTime) {
				continue
			}
			// For incremental sync, only include modified after 'since'
			if incremental && !state.ModifiedAt.IsZero() && !state.ModifiedAt.After(since) {
				continue
			}
			states[ip] = state
		}
		p.GetPersistenceMutex().Unlock()

		// Build response
		response := StateSyncResponse{
			Version:   StateSyncVersion,
			Timestamp: time.Now(),
			States:    states,
		}

		// Set content type
		w.Header().Set("Content-Type", "application/json")

		// Apply compression if requested and enabled
		_, useCompression, _, _ := p.GetStateSyncConfig()
		acceptsGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")

		if useCompression && acceptsGzip {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			defer func() { _ = gz.Close() }()
			if err := json.NewEncoder(gz).Encode(response); err != nil {
				p.Log(logging.LevelError, "STATE_SYNC", "Failed to encode state response: %v", err)
			}
		} else {
			if err := json.NewEncoder(w).Encode(response); err != nil {
				p.Log(logging.LevelError, "STATE_SYNC", "Failed to encode state response: %v", err)
			}
		}
	}
}

// clusterMergedStateHandler exposes the cluster-wide merged state (leader only).
// GET /api/v1/cluster/state/merged?since=<timestamp>
func clusterMergedStateHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Only leader can provide merged state
		if p.GetNodeRole() != "leader" {
			http.Error(w, "Only leader provides merged state", http.StatusForbidden)
			return
		}

		// Parse optional "since" parameter for incremental sync
		sinceStr := r.URL.Query().Get("since")
		var since time.Time
		if sinceStr != "" {
			var err error
			since, err = time.Parse(time.RFC3339, sinceStr)
			if err != nil {
				http.Error(w, "Invalid 'since' timestamp format", http.StatusBadRequest)
				return
			}
		}

		// Collect and merge states from all nodes
		merged, nodesQueried, nodesFailed := collectAndMergeStates(p, since)

		// Build response
		response := MergedStateResponse{
			Version:      StateSyncVersion,
			Timestamp:    time.Now(),
			NodesQueried: nodesQueried,
			NodesFailed:  nodesFailed,
			States:       merged,
		}

		// Set content type
		w.Header().Set("Content-Type", "application/json")

		// Apply compression if requested and enabled
		_, useCompression, _, _ := p.GetStateSyncConfig()
		acceptsGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")

		if useCompression && acceptsGzip {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			defer func() { _ = gz.Close() }()
			if err := json.NewEncoder(gz).Encode(response); err != nil {
				p.Log(logging.LevelError, "STATE_SYNC", "Failed to encode merged state response: %v", err)
			}
		} else {
			if err := json.NewEncoder(w).Encode(response); err != nil {
				p.Log(logging.LevelError, "STATE_SYNC", "Failed to encode merged state response: %v", err)
			}
		}
	}
}

// collectAndMergeStates queries all nodes and merges their IPStates.
func collectAndMergeStates(p Provider, since time.Time) (map[string]persistence.IPState, []string, []string) {
	merged := make(map[string]persistence.IPState)
	var nodesQueried []string
	var nodesFailed []string

	clusterConfigInterface := p.GetClusterConfig()
	if clusterConfigInterface == nil {
		return merged, nodesQueried, nodesFailed
	}

	// Use reflection to access cluster config fields
	v := reflect.ValueOf(clusterConfigInterface)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	nodesField := v.FieldByName("Nodes")
	protocolField := v.FieldByName("Protocol")
	stateSyncField := v.FieldByName("StateSync")

	if !nodesField.IsValid() || !protocolField.IsValid() || !stateSyncField.IsValid() {
		return merged, nodesQueried, nodesFailed
	}

	protocol := protocolField.String()
	timeout := stateSyncField.FieldByName("Timeout").Interface().(time.Duration)
	incremental := stateSyncField.FieldByName("Incremental").Bool()

	nodeName := p.GetNodeName()

	// Add leader's own state first
	p.GetPersistenceMutex().Lock()
	for ip, state := range p.GetIPStates() {
		// Skip expired entries
		if !state.ExpireTime.IsZero() && time.Now().After(state.ExpireTime) {
			continue
		}
		// For incremental, filter by modification time
		if incremental && !since.IsZero() && !state.ModifiedAt.IsZero() && !state.ModifiedAt.After(since) {
			continue
		}
		// Add source node to reason
		state.Reason = addSourceNode(state.Reason, nodeName, "")
		merged[ip] = state
	}
	p.GetPersistenceMutex().Unlock()
	nodesQueried = append(nodesQueried, nodeName)

	// Query each follower
	client := &http.Client{Timeout: timeout}
	for i := 0; i < nodesField.Len(); i++ {
		nodeVal := nodesField.Index(i)
		nodeNameField := nodeVal.FieldByName("Name").String()
		nodeAddressField := nodeVal.FieldByName("Address").String()

		if nodeNameField == nodeName {
			continue // Skip self
		}

		url := protocol + "://" + nodeAddressField + "/api/v1/cluster/internal/persistence/state"
		if incremental && !since.IsZero() {
			url += "?since=" + since.Format(time.RFC3339)
		}

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			p.Log(logging.LevelWarning, "STATE_MERGE", "Failed to create request for %s: %v", nodeNameField, err)
			nodesFailed = append(nodesFailed, nodeNameField)
			continue
		}

		// Request gzip compression if enabled (always request, let server decide)
		req.Header.Set("Accept-Encoding", "gzip")

		resp, err := client.Do(req)
		if err != nil {
			p.Log(logging.LevelWarning, "STATE_MERGE", "Failed to fetch state from %s: %v", nodeNameField, err)
			nodesFailed = append(nodesFailed, nodeNameField)
			continue
		}

		// Check if response is gzipped
		var reader = resp.Body
		if resp.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(resp.Body)
			if err != nil {
				p.Log(logging.LevelWarning, "STATE_MERGE", "Failed to create gzip reader for %s: %v", nodeNameField, err)
				_ = resp.Body.Close()
				nodesFailed = append(nodesFailed, nodeNameField)
				continue
			}
			defer func() { _ = gz.Close() }()
			reader = gz
		}

		var stateResp StateSyncResponse
		if err := json.NewDecoder(reader).Decode(&stateResp); err != nil {
			p.Log(logging.LevelWarning, "STATE_MERGE", "Failed to decode state from %s: %v", nodeNameField, err)
			_ = resp.Body.Close()
			nodesFailed = append(nodesFailed, nodeNameField)
			continue
		}
		_ = resp.Body.Close()

		// Check version compatibility
		if stateResp.Version != StateSyncVersion {
			p.Log(logging.LevelWarning, "STATE_MERGE", "Version mismatch from %s: got %s, expected %s", nodeNameField, stateResp.Version, StateSyncVersion)
			// Continue anyway for backward compatibility
		}

		nodesQueried = append(nodesQueried, nodeNameField)

		// Merge states with conflict resolution
		for ip, state := range stateResp.States {
			// Add source node to reason
			state.Reason = addSourceNode(state.Reason, nodeNameField, nodeAddressField)

			if existing, ok := merged[ip]; ok {
				// Merge reasons (prevent duplication)
				state.Reason = mergeReasons(existing.Reason, state.Reason)

				// Keep longer expiry
				if existing.ExpireTime.After(state.ExpireTime) {
					state.ExpireTime = existing.ExpireTime
				}
				// Keep latest modification time
				if state.ModifiedAt.After(existing.ModifiedAt) {
					existing.ModifiedAt = state.ModifiedAt
				}
			}
			merged[ip] = state
		}
	}

	return merged, nodesQueried, nodesFailed
}

// addSourceNode adds source node attribution to a reason if not already present.
func addSourceNode(reason, nodeName, nodeAddress string) string {
	if strings.Contains(reason, " (") && strings.HasSuffix(reason, ")") {
		return reason
	}
	source := nodeName
	if source == "" {
		source = nodeAddress
	}
	return reason + " (" + source + ")"
}

// mergeReasons combines two reasons without duplication.
func mergeReasons(existing, newReason string) string {
	if existing == "" {
		return newReason
	}
	if newReason == "" {
		return existing
	}

	// Parse existing reasons into map
	reasonMap := make(map[string]bool)
	for _, part := range strings.Split(existing, " | ") {
		baseReason := extractBaseReason(strings.TrimSpace(part))
		reasonMap[baseReason] = true
	}

	newBaseReason := extractBaseReason(newReason)
	if !reasonMap[newBaseReason] {
		return existing + " | " + newReason
	}
	return existing
}

// extractBaseReason extracts the reason without source node attribution.
func extractBaseReason(reason string) string {
	if idx := strings.Index(reason, " ("); idx != -1 {
		return reason[:idx]
	}
	return reason
}
