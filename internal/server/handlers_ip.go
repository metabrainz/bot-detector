package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strings"
	"time"

	"bot-detector/internal/logging"
	"bot-detector/internal/store"
	"bot-detector/internal/utils"
)

// ipLookupHandler returns local IP status in plain text
func ipLookupHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ipStr := r.PathValue("ip")
		if ipStr == "" {
			http.Error(w, "IP address required", http.StatusBadRequest)
			return
		}

		// Canonicalize IP
		ip := net.ParseIP(ipStr)
		if ip == nil {
			http.Error(w, "Invalid IP address", http.StatusBadRequest)
			return
		}
		canonical := ip.String()

		// Search ActivityStore
		p.GetActivityMutex().RLock()
		activityStore := p.GetActivityStore()

		// Collect all actors with this IP
		var actors []*store.ActorActivity
		for actor, activity := range activityStore {
			if actor.IPInfo.Address == canonical {
				actors = append(actors, activity)
			}
		}
		p.GetActivityMutex().RUnlock()

		// Check HAProxy tables
		var inBackend bool
		blockerInterface := p.GetBlocker()
		if blockerInterface != nil {
			if b, ok := blockerInterface.(interface {
				IsIPBlocked(ipInfo utils.IPInfo) (bool, error)
			}); ok {
				ipInfo := utils.NewIPInfo(canonical)
				if blocked, err := b.IsIPBlocked(ipInfo); err == nil {
					inBackend = blocked
				}
			}
		}

		// Format response
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		nodeName := p.GetNodeName()
		if nodeName != "" {
			_, _ = fmt.Fprintf(w, "node: %s\n", nodeName)
		}

		if len(actors) == 0 && !inBackend {
			_, _ = fmt.Fprint(w, "status: unknown\n")
			addFollowerHint(w, p, canonical)
			return
		}

		// If in HAProxy but not in activity store, show as blocked
		if inBackend && len(actors) == 0 {
			_, _ = fmt.Fprint(w, "status: blocked\n")
			_, _ = fmt.Fprint(w, "source: backend\n")
			addFollowerHint(w, p, canonical)
			return
		}

		// Aggregate status from activity store
		status := aggregateActorStatus(actors)
		_, _ = fmt.Fprint(w, status)

		// Add HAProxy status if different
		if inBackend {
			_, _ = fmt.Fprint(w, "backend: present\n")
		}

		addFollowerHint(w, p, canonical)
	}
}

// aggregateActorStatus combines multiple actor activities into a status string
func aggregateActorStatus(actors []*store.ActorActivity) string {
	var result string

	// Check if any actor is blocked
	var blockedActors []*store.ActorActivity
	for _, a := range actors {
		if a.IsBlocked && time.Now().Before(a.BlockedUntil) {
			blockedActors = append(blockedActors, a)
		}
	}

	if len(blockedActors) > 0 {
		result += "status: blocked\n"
		result += fmt.Sprintf("actors: %d\n", len(actors))

		// Collect unique chains and timing
		chains := make(map[string]time.Time)
		var earliestBlock time.Time
		var latestExpiry time.Time

		for _, a := range blockedActors {
			if a.SkipInfo.Source != "" {
				if existing, ok := chains[a.SkipInfo.Source]; !ok || a.BlockedUntil.After(existing) {
					chains[a.SkipInfo.Source] = a.BlockedUntil
				}
			}

			// Estimate block time (actual block time not stored, use BlockedUntil - duration)
			// This is approximate since we don't store the original block time
			if earliestBlock.IsZero() || a.BlockedUntil.Before(latestExpiry) {
				earliestBlock = a.BlockedUntil.Add(-1 * time.Hour) // Rough estimate
			}
			if latestExpiry.IsZero() || a.BlockedUntil.After(latestExpiry) {
				latestExpiry = a.BlockedUntil
			}
		}

		if len(chains) > 0 {
			result += "chains:\n"
			for chain, expiry := range chains {
				result += fmt.Sprintf("  - %s (until: %s)\n", chain, expiry.Format(time.RFC3339))
			}
		}

		if !earliestBlock.IsZero() {
			result += fmt.Sprintf("earliest_block: %s\n", earliestBlock.Format(time.RFC3339))
		}
		if !latestExpiry.IsZero() {
			result += fmt.Sprintf("latest_expiry: %s\n", latestExpiry.Format(time.RFC3339))
		}
	} else {
		// Not blocked - find most recent activity
		var mostRecent *store.ActorActivity
		for _, a := range actors {
			if mostRecent == nil || a.LastRequestTime.After(mostRecent.LastRequestTime) {
				mostRecent = a
			}
		}

		result += "status: unblocked\n"
		if !mostRecent.LastRequestTime.IsZero() {
			result += fmt.Sprintf("last_seen: %s\n", mostRecent.LastRequestTime.Format(time.RFC3339))
		}
		if !mostRecent.LastUnblockTime.IsZero() {
			result += fmt.Sprintf("last_unblock: %s\n", mostRecent.LastUnblockTime.Format(time.RFC3339))
			if mostRecent.LastUnblockReason != "" {
				result += fmt.Sprintf("reason: %s\n", mostRecent.LastUnblockReason)
			}
		}
	}

	return result
}

// addFollowerHint adds a note about cluster endpoint if this is a follower
func addFollowerHint(w http.ResponseWriter, p Provider, ip string) {
	if p.GetNodeRole() == "follower" {
		leaderAddr := p.GetNodeLeaderAddress()
		if leaderAddr != "" {
			protocol := p.GetClusterProtocol()
			_, _ = fmt.Fprintf(w, "note: For cluster-wide view, query leader at %s://%s/cluster/ip/%s\n",
				protocol, leaderAddr, ip)
		}
	}
}

// IPStatusResponse is the JSON response for IP status queries
type IPStatusResponse struct {
	Node          string            `json:"node,omitempty"`
	Status        string            `json:"status"` // "blocked", "unblocked", "unknown"
	Actors        int               `json:"actors,omitempty"`
	Chains        map[string]string `json:"chains,omitempty"`         // chain -> expiry time (RFC3339)
	EarliestBlock string            `json:"earliest_block,omitempty"` // RFC3339
	LatestExpiry  string            `json:"latest_expiry,omitempty"`  // RFC3339
	LastSeen      string            `json:"last_seen,omitempty"`      // RFC3339
	LastUnblock   string            `json:"last_unblock,omitempty"`   // RFC3339
	UnblockReason string            `json:"unblock_reason,omitempty"`
	Backend       string            `json:"backend,omitempty"`      // "present" if in HAProxy tables
	ClusterHint   string            `json:"cluster_hint,omitempty"` // URL to cluster endpoint
}

// apiIPLookupHandler returns local IP status as JSON
func apiIPLookupHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ipStr := r.PathValue("ip")
		if ipStr == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":"IP address required"}`)
			return
		}

		// Canonicalize IP
		ip := net.ParseIP(ipStr)
		if ip == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":"Invalid IP address"}`)
			return
		}
		canonical := ip.String()

		// Search ActivityStore
		p.GetActivityMutex().RLock()
		activityStore := p.GetActivityStore()

		// Collect all actors with this IP
		var actors []*store.ActorActivity
		for actor, activity := range activityStore {
			if actor.IPInfo.Address == canonical {
				actors = append(actors, activity)
			}
		}
		p.GetActivityMutex().RUnlock()

		// Check HAProxy tables
		var inBackend bool
		blockerInterface := p.GetBlocker()
		if blockerInterface != nil {
			if b, ok := blockerInterface.(interface {
				IsIPBlocked(ipInfo utils.IPInfo) (bool, error)
			}); ok {
				ipInfo := utils.NewIPInfo(canonical)
				if blocked, err := b.IsIPBlocked(ipInfo); err == nil {
					inBackend = blocked
				}
			}
		}

		// Build response
		response := buildIPStatusResponse(p, actors, canonical, inBackend)

		// Return JSON
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			p.Log(logging.LevelError, "API", "Failed to encode JSON response: %v", err)
		}
	}
}

// buildIPStatusResponse creates JSON response from actor activities
func buildIPStatusResponse(p Provider, actors []*store.ActorActivity, ip string, inBackend bool) IPStatusResponse {
	response := IPStatusResponse{
		Node: p.GetNodeName(),
	}

	if len(actors) == 0 && !inBackend {
		response.Status = "unknown"
		addClusterHintJSON(&response, p, ip)
		return response
	}

	// If in HAProxy but not in activity store
	if inBackend && len(actors) == 0 {
		response.Status = "blocked"
		response.Backend = "present"
		addClusterHintJSON(&response, p, ip)
		return response
	}

	// Check if any actor is blocked
	var blockedActors []*store.ActorActivity
	for _, a := range actors {
		if a.IsBlocked && time.Now().Before(a.BlockedUntil) {
			blockedActors = append(blockedActors, a)
		}
	}

	if len(blockedActors) > 0 {
		response.Status = "blocked"
		response.Actors = len(actors)

		// Collect unique chains and timing
		chains := make(map[string]string)
		var earliestBlock time.Time
		var latestExpiry time.Time

		for _, a := range blockedActors {
			if a.SkipInfo.Source != "" {
				expiry := a.BlockedUntil.Format(time.RFC3339)
				if existing, ok := chains[a.SkipInfo.Source]; !ok || a.BlockedUntil.After(parseRFC3339(existing)) {
					chains[a.SkipInfo.Source] = expiry
				}
			}

			// Estimate block time
			blockTime := a.BlockedUntil.Add(-1 * time.Hour)
			if earliestBlock.IsZero() || blockTime.Before(earliestBlock) {
				earliestBlock = blockTime
			}
			if latestExpiry.IsZero() || a.BlockedUntil.After(latestExpiry) {
				latestExpiry = a.BlockedUntil
			}
		}

		if len(chains) > 0 {
			response.Chains = chains
		}
		if !earliestBlock.IsZero() {
			response.EarliestBlock = earliestBlock.Format(time.RFC3339)
		}
		if !latestExpiry.IsZero() {
			response.LatestExpiry = latestExpiry.Format(time.RFC3339)
		}

		if inBackend {
			response.Backend = "present"
		}
	} else {
		// Not blocked - find most recent activity
		var mostRecent *store.ActorActivity
		for _, a := range actors {
			if mostRecent == nil || a.LastRequestTime.After(mostRecent.LastRequestTime) {
				mostRecent = a
			}
		}

		response.Status = "unblocked"
		if !mostRecent.LastRequestTime.IsZero() {
			response.LastSeen = mostRecent.LastRequestTime.Format(time.RFC3339)
		}
		if !mostRecent.LastUnblockTime.IsZero() {
			response.LastUnblock = mostRecent.LastUnblockTime.Format(time.RFC3339)
			response.UnblockReason = mostRecent.LastUnblockReason
		}

		if inBackend {
			response.Backend = "present"
		}
	}

	addClusterHintJSON(&response, p, ip)
	return response
}

// addClusterHintJSON adds cluster endpoint hint for followers
func addClusterHintJSON(response *IPStatusResponse, p Provider, ip string) {
	if p.GetNodeRole() == "follower" {
		leaderAddr := p.GetNodeLeaderAddress()
		if leaderAddr != "" {
			protocol := p.GetClusterProtocol()
			response.ClusterHint = fmt.Sprintf("%s://%s/cluster/ip/%s", protocol, leaderAddr, ip)
		}
	}
}

// parseRFC3339 is a helper to parse RFC3339 time strings
func parseRFC3339(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// unblockIPHandler unblocks an IP by removing it from all HAProxy tables on all backends
func unblockIPHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ipStr := r.PathValue("ip")
		if ipStr == "" {
			http.Error(w, "IP address required", http.StatusBadRequest)
			return
		}

		// Canonicalize IP
		ip := net.ParseIP(ipStr)
		if ip == nil {
			http.Error(w, "Invalid IP address", http.StatusBadRequest)
			return
		}
		canonical := ip.String()

		// Get blocker
		blockerInterface := p.GetBlocker()
		if blockerInterface == nil {
			http.Error(w, "Blocker not available", http.StatusServiceUnavailable)
			return
		}

		// Type assert to blocker with ClearIP method
		b, ok := blockerInterface.(interface {
			ClearIP(ipInfo utils.IPInfo) ([]interface{}, error)
		})
		if !ok {
			http.Error(w, "Blocker interface error", http.StatusInternalServerError)
			return
		}

		// Create IPInfo using helper
		ipInfo := utils.NewIPInfo(canonical)

		// Clear the IP from all tables
		foundInfo, err := b.ClearIP(ipInfo)
		if err != nil {
			p.Log(logging.LevelError, "API", "Failed to clear IP %s: %v", canonical, err)
			http.Error(w, fmt.Sprintf("Failed to clear IP: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)

		if len(foundInfo) == 0 {
			_, _ = fmt.Fprintf(w, "IP %s not found in any tables\n", canonical)
			p.Log(logging.LevelInfo, "API", "IP %s not found in any tables", canonical)
			return
		}

		// Get duration tables for calculating when IP was added
		durationTables := p.GetDurationTables()

		_, _ = fmt.Fprintf(w, "IP %s found and cleared from:\n", canonical)
		for _, info := range foundInfo {
			// Use reflection to access fields since we can't import blocker package
			infoVal := reflect.ValueOf(info)
			tableName := infoVal.FieldByName("TableName").String()
			backend := infoVal.FieldByName("Backend").String()
			gpc0 := int(infoVal.FieldByName("Gpc0").Int())
			expMillis := infoVal.FieldByName("ExpMillis").Int()

			status := "unblocked"
			if gpc0 > 0 {
				status = "blocked"
			}

			expSec := expMillis / 1000
			expiresDuration := time.Duration(expSec) * time.Second

			// Find table duration by matching table name
			var tableDuration time.Duration
			for dur, baseTableName := range durationTables {
				if strings.HasPrefix(tableName, baseTableName) {
					tableDuration = dur
					break
				}
			}

			if tableDuration > 0 {
				// Calculate when it was added: now - (duration - expires)
				elapsedSec := tableDuration.Seconds() - float64(expSec)
				addedAt := time.Now().Add(-time.Duration(elapsedSec) * time.Second)
				_, _ = fmt.Fprintf(w, "  - %s on %s (status: %s, duration: %v, expires in: %s, added: %s)\n",
					tableName, backend, status, tableDuration, formatDuration(expiresDuration), addedAt.Format("2006-01-02 15:04:05"))
			} else {
				_, _ = fmt.Fprintf(w, "  - %s on %s (status: %s, expires in: %s)\n",
					tableName, backend, status, formatDuration(expiresDuration))
			}
		}

		p.Log(logging.LevelInfo, "API", "IP %s cleared from %d table entries", canonical, len(foundInfo))
	}
}

// formatDuration formats a duration in human-readable format (weeks, days, hours, minutes, seconds)
func formatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}

	seconds := int64(d.Seconds())

	weeks := seconds / 604800
	seconds %= 604800
	days := seconds / 86400
	seconds %= 86400
	hours := seconds / 3600
	seconds %= 3600
	minutes := seconds / 60
	seconds %= 60

	var parts []string
	if weeks > 0 {
		parts = append(parts, fmt.Sprintf("%dw", weeks))
	}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if seconds > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}

	return strings.Join(parts, " ")
}

// clusterIPLookupHandler returns IP status as JSON for internal cluster queries
// This endpoint is used by the leader to query follower nodes
func clusterIPLookupHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ipStr := r.PathValue("ip")
		if ipStr == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":"IP address required"}`)
			return
		}

		// Canonicalize IP
		ip := net.ParseIP(ipStr)
		if ip == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":"Invalid IP address"}`)
			return
		}
		canonical := ip.String()

		// Search ActivityStore
		p.GetActivityMutex().RLock()
		activityStore := p.GetActivityStore()

		// Collect all actors with this IP
		var actors []*store.ActorActivity
		for actor, activity := range activityStore {
			if actor.IPInfo.Address == canonical {
				actors = append(actors, activity)
			}
		}
		p.GetActivityMutex().RUnlock()

		// Check HAProxy tables
		var inBackend bool
		blockerInterface := p.GetBlocker()
		if blockerInterface != nil {
			if b, ok := blockerInterface.(interface {
				IsIPBlocked(ipInfo utils.IPInfo) (bool, error)
			}); ok {
				ipInfo := utils.NewIPInfo(canonical)
				if blocked, err := b.IsIPBlocked(ipInfo); err == nil {
					inBackend = blocked
				}
			}
		}

		// Build response (without node name and cluster hint for internal use)
		response := buildIPStatusResponse(p, actors, canonical, inBackend)
		response.Node = ""        // Leader will add node name
		response.ClusterHint = "" // Not needed for internal queries

		// Return JSON
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			p.Log(logging.LevelError, "CLUSTER", "Failed to encode JSON response: %v", err)
		}
	}
}

// ClusterIPAggregateResponse is the aggregated IP status across all cluster nodes
type ClusterIPAggregateResponse struct {
	ClusterStatus string                 `json:"cluster_status"` // "blocked", "unblocked", "unknown", "mixed"
	Nodes         []NodeIPStatusResponse `json:"nodes"`
}

// NodeIPStatusResponse is the IP status for a single node
type NodeIPStatusResponse struct {
	Name          string            `json:"name"`
	Status        string            `json:"status"` // "blocked", "unblocked", "unknown", "error"
	Error         string            `json:"error,omitempty"`
	Actors        int               `json:"actors,omitempty"`
	Chains        map[string]string `json:"chains,omitempty"`
	EarliestBlock string            `json:"earliest_block,omitempty"`
	LatestExpiry  string            `json:"latest_expiry,omitempty"`
	LastSeen      string            `json:"last_seen,omitempty"`
	LastUnblock   string            `json:"last_unblock,omitempty"`
	UnblockReason string            `json:"unblock_reason,omitempty"`
}

// NodeInfo is a minimal representation of cluster node to avoid import cycles
type NodeInfo struct {
	Name    string
	Address string
}

// clusterIPAggregateHandler queries all nodes and returns aggregated status in plain text (leader only)
func clusterIPAggregateHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check if leader
		if p.GetNodeRole() != "leader" {
			http.Error(w, "Cluster IP aggregation only available on leader nodes", http.StatusNotFound)
			return
		}

		// Extract and validate IP
		ipStr := r.PathValue("ip")
		if ipStr == "" {
			http.Error(w, "IP address required", http.StatusBadRequest)
			return
		}

		// Canonicalize IP
		ip := net.ParseIP(ipStr)
		if ip == nil {
			http.Error(w, "Invalid IP address", http.StatusBadRequest)
			return
		}
		canonical := ip.String()

		// Get cluster nodes and query them
		response := queryAllNodes(p, canonical)

		// Format as plain text
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "cluster_status: %s\n", response.ClusterStatus)
		_, _ = fmt.Fprint(w, "nodes:\n")
		for _, node := range response.Nodes {
			_, _ = fmt.Fprintf(w, "  - name: %s\n", node.Name)
			_, _ = fmt.Fprintf(w, "    status: %s\n", node.Status)
			if node.Error != "" {
				_, _ = fmt.Fprintf(w, "    error: %s\n", node.Error)
			}
			if node.Actors > 0 {
				_, _ = fmt.Fprintf(w, "    actors: %d\n", node.Actors)
			}
			if len(node.Chains) > 0 {
				_, _ = fmt.Fprint(w, "    chains:\n")
				for chain, expiry := range node.Chains {
					_, _ = fmt.Fprintf(w, "      - %s (until: %s)\n", chain, expiry)
				}
			}
			if node.EarliestBlock != "" {
				_, _ = fmt.Fprintf(w, "    earliest_block: %s\n", node.EarliestBlock)
			}
			if node.LatestExpiry != "" {
				_, _ = fmt.Fprintf(w, "    latest_expiry: %s\n", node.LatestExpiry)
			}
			if node.LastSeen != "" {
				_, _ = fmt.Fprintf(w, "    last_seen: %s\n", node.LastSeen)
			}
			if node.LastUnblock != "" {
				_, _ = fmt.Fprintf(w, "    last_unblock: %s\n", node.LastUnblock)
			}
			if node.UnblockReason != "" {
				_, _ = fmt.Fprintf(w, "    reason: %s\n", node.UnblockReason)
			}
		}
	}
}

// apiClusterIPAggregateHandler queries all nodes and returns aggregated status as JSON (leader only)
func apiClusterIPAggregateHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check if leader
		if p.GetNodeRole() != "leader" {
			http.Error(w, "Cluster IP aggregation only available on leader nodes", http.StatusNotFound)
			return
		}

		// Extract and validate IP
		ipStr := r.PathValue("ip")
		if ipStr == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":"IP address required"}`)
			return
		}

		// Canonicalize IP
		ip := net.ParseIP(ipStr)
		if ip == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":"Invalid IP address"}`)
			return
		}
		canonical := ip.String()

		// Get cluster nodes and query them
		response := queryAllNodes(p, canonical)

		// Return JSON
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			p.Log(logging.LevelError, "CLUSTER", "Failed to encode cluster IP response: %v", err)
		}
	}
}

// queryAllNodes queries all cluster nodes and aggregates their responses
func queryAllNodes(p Provider, canonical string) ClusterIPAggregateResponse {
	nodes := extractNodeInfo(p.GetClusterNodes())
	protocol := p.GetClusterProtocol()

	response := ClusterIPAggregateResponse{
		Nodes: make([]NodeIPStatusResponse, 0, len(nodes)),
	}

	// Track overall cluster status
	hasBlocked := false
	hasUnblocked := false
	hasUnknown := false

	for _, node := range nodes {
		nodeResponse := queryNodeIPStatus(p, node.Name, node.Address, protocol, canonical)
		response.Nodes = append(response.Nodes, nodeResponse)

		// Track status for cluster-wide determination
		switch nodeResponse.Status {
		case "blocked":
			hasBlocked = true
		case "unblocked":
			hasUnblocked = true
		case "unknown":
			hasUnknown = true
		}
	}

	// Determine cluster-wide status
	if hasBlocked {
		if hasUnblocked || hasUnknown {
			response.ClusterStatus = "mixed"
		} else {
			response.ClusterStatus = "blocked"
		}
	} else if hasUnblocked {
		response.ClusterStatus = "unblocked"
	} else {
		response.ClusterStatus = "unknown"
	}

	return response
}

// extractNodeInfo extracts node information from interface{} to avoid import cycles
func extractNodeInfo(nodesInterface interface{}) []NodeInfo {
	if nodesInterface == nil {
		return nil
	}

	// The actual type is []cluster.NodeConfig, but we can't import cluster here
	// Use reflection to extract Name and Address fields
	v := reflect.ValueOf(nodesInterface)
	if v.Kind() != reflect.Slice {
		return nil
	}

	nodes := make([]NodeInfo, v.Len())
	for i := 0; i < v.Len(); i++ {
		elem := v.Index(i)
		nodes[i] = NodeInfo{
			Name:    elem.FieldByName("Name").String(),
			Address: elem.FieldByName("Address").String(),
		}
	}

	return nodes
}

// queryNodeIPStatus queries a single node for IP status
func queryNodeIPStatus(p Provider, nodeName, nodeAddr, protocol, ip string) NodeIPStatusResponse {
	url := fmt.Sprintf("%s://%s/api/v1/cluster/internal/ip/%s", protocol, nodeAddr, ip)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		p.Log(logging.LevelWarning, "CLUSTER", "Failed to query node %s: %v", nodeName, err)
		return NodeIPStatusResponse{
			Name:   nodeName,
			Status: "error",
			Error:  err.Error(),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		p.Log(logging.LevelWarning, "CLUSTER", "Node %s returned status %d", nodeName, resp.StatusCode)
		return NodeIPStatusResponse{
			Name:   nodeName,
			Status: "error",
			Error:  fmt.Sprintf("HTTP %d", resp.StatusCode),
		}
	}

	var status IPStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		p.Log(logging.LevelWarning, "CLUSTER", "Failed to decode response from node %s: %v", nodeName, err)
		return NodeIPStatusResponse{
			Name:   nodeName,
			Status: "error",
			Error:  "decode error",
		}
	}

	// Convert to NodeIPStatusResponse
	return NodeIPStatusResponse{
		Name:          nodeName,
		Status:        status.Status,
		Actors:        status.Actors,
		Chains:        status.Chains,
		EarliestBlock: status.EarliestBlock,
		LatestExpiry:  status.LatestExpiry,
		LastSeen:      status.LastSeen,
		LastUnblock:   status.LastUnblock,
		UnblockReason: status.UnblockReason,
	}
}
