package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"time"

	"bot-detector/internal/blocker"
	"bot-detector/internal/logging"
	"bot-detector/internal/store"
	"bot-detector/internal/utils"
)

// ipLookupHandler returns IP status in plain text (cluster-aware)
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

		// If follower, forward to leader for cluster-wide view
		if p.GetNodeRole() == "follower" {
			path := fmt.Sprintf("/ip/%s", canonical)
			resp, err := forwardToLeader(p, "GET", path, nil)
			if err != nil {
				p.Log(logging.LevelError, "IP_LOOKUP", "Failed to forward to leader: %v", err)
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer func() { _ = resp.Body.Close() }()

			// Copy response from leader
			w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			return
		}

		// Leader or standalone: return cluster-wide or local view
		if p.GetNodeRole() == "leader" {
			// Return cluster-wide aggregated view
			renderClusterIPStatus(w, p, canonical)
			return
		}

		// Standalone: return local view
		renderLocalIPStatus(w, p, canonical)
	}
}

// renderLocalIPStatus renders local node IP status in plain text
func renderLocalIPStatus(w http.ResponseWriter, p Provider, canonical string) {
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

	// Check HAProxy tables for detailed information
	var backendInfo []interface{}
	blockerInterface := p.GetBlocker()
	if blockerInterface != nil {
		// Try to get detailed IP information from HAProxy
		// We use reflection to avoid import cycles with the blocker package
		getDetailsMethod := reflect.ValueOf(blockerInterface).MethodByName("GetIPDetails")
		if getDetailsMethod.IsValid() {
			ipInfo := utils.NewIPInfo(canonical)
			results := getDetailsMethod.Call([]reflect.Value{reflect.ValueOf(ipInfo)})
			p.Log(logging.LevelInfo, "IP_LOOKUP", "GetIPDetails called for %s, results: %d", canonical, len(results))
			if len(results) == 2 && results[1].IsNil() { // No error
				// Convert result to []interface{}
				detailsVal := results[0]
				p.Log(logging.LevelInfo, "IP_LOOKUP", "Details value kind: %v, len: %d", detailsVal.Kind(), detailsVal.Len())
				if detailsVal.Kind() == reflect.Slice {
					for i := 0; i < detailsVal.Len(); i++ {
						backendInfo = append(backendInfo, detailsVal.Index(i).Interface())
					}
				}
				p.Log(logging.LevelInfo, "IP_LOOKUP", "Backend info collected: %d entries", len(backendInfo))
			} else if len(results) == 2 && !results[1].IsNil() {
				err := results[1].Interface().(error)
				p.Log(logging.LevelInfo, "IP_LOOKUP", "Failed to get backend details for %s: %v", canonical, err)
			}
		} else {
			p.Log(logging.LevelInfo, "IP_LOOKUP", "GetIPDetails method not found on blocker")
		}
	} else {
		p.Log(logging.LevelInfo, "IP_LOOKUP", "Blocker interface is nil")
	}

	// Format response
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	nodeName := p.GetNodeName()
	if nodeName != "" {
		_, _ = fmt.Fprintf(w, "node: %s\n", nodeName)
	}

	// Check persistence state
	persistState, hasPersist := p.GetPersistenceState(canonical)

	if len(actors) == 0 && len(backendInfo) == 0 {
		_, _ = fmt.Fprint(w, "status: unknown\n")
		formatPersistenceState(w, persistState, hasPersist)
		return
	}

	// If in HAProxy but not in activity store, show backend details
	if len(backendInfo) > 0 && len(actors) == 0 {
		_, _ = fmt.Fprint(w, "status: blocked\n")
		_, _ = fmt.Fprint(w, "source: backend\n")
		formatBackendInfo(w, backendInfo, p.GetDurationTables())
		formatPersistenceState(w, persistState, hasPersist)
		// If no persistence reason available, add note
		if !hasPersist || reflect.ValueOf(persistState).FieldByName("Reason").String() == "" {
			_, _ = fmt.Fprint(w, "note: Block reason unavailable (IP may have been manually added to HAProxy or blocked before persistence was enabled)\n")
		}
		return
	}

	// Aggregate status from activity store
	status := aggregateActorStatus(actors, persistState, hasPersist)
	_, _ = fmt.Fprint(w, status)

	// Add HAProxy details if present
	if len(backendInfo) > 0 {
		_, _ = fmt.Fprint(w, "backend_tables:\n")
		formatBackendInfo(w, backendInfo, p.GetDurationTables())
	}

	formatPersistenceState(w, persistState, hasPersist)
}

// renderClusterIPStatus renders cluster-wide aggregated IP status in plain text
func renderClusterIPStatus(w http.ResponseWriter, p Provider, canonical string) {
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
		if node.Persistence != "" {
			_, _ = fmt.Fprintf(w, "    persistence: %s\n", node.Persistence)
		}
		if node.PersistenceExpires != "" {
			_, _ = fmt.Fprintf(w, "    persistence_expires: %s\n", node.PersistenceExpires)
		}
		if node.PersistenceReason != "" {
			_, _ = fmt.Fprintf(w, "    persistence_reason: %s\n", node.PersistenceReason)
		}
	}

	// Add backend table details from leader node
	blockerInterface := p.GetBlocker()
	if blockerInterface != nil {
		getDetailsMethod := reflect.ValueOf(blockerInterface).MethodByName("GetIPDetails")
		if getDetailsMethod.IsValid() {
			ipInfo := utils.NewIPInfo(canonical)
			results := getDetailsMethod.Call([]reflect.Value{reflect.ValueOf(ipInfo)})
			if len(results) == 2 && results[1].IsNil() {
				detailsVal := results[0]
				if detailsVal.Kind() == reflect.Slice && detailsVal.Len() > 0 {
					_, _ = fmt.Fprint(w, "backend_tables:\n")
					for i := 0; i < detailsVal.Len(); i++ {
						info := detailsVal.Index(i).Interface()
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

						var tableDuration time.Duration
						for dur, baseTableName := range p.GetDurationTables() {
							if strings.HasPrefix(tableName, baseTableName) {
								tableDuration = dur
								break
							}
						}

						if tableDuration > 0 {
							elapsedSec := tableDuration.Seconds() - float64(expSec)
							addedAt := time.Now().Add(-time.Duration(elapsedSec) * time.Second)
							_, _ = fmt.Fprintf(w, "  - %s on %s (status: %s, duration: %v, expires in: %s, added: %s)\n",
								tableName, backend, status, tableDuration, formatDuration(expiresDuration), addedAt.Format("2006-01-02 15:04:05"))
						} else {
							_, _ = fmt.Fprintf(w, "  - %s on %s (status: %s, expires in: %s)\n",
								tableName, backend, status, formatDuration(expiresDuration))
						}
					}
				}
			}
		}
	}
}

// formatBackendInfo formats HAProxy table information for display
func formatBackendInfo(w http.ResponseWriter, backendInfo []interface{}, durationTables map[time.Duration]string) {
	for _, info := range backendInfo {
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

		// Find table duration
		var tableDuration time.Duration
		for dur, baseTableName := range durationTables {
			if strings.HasPrefix(tableName, baseTableName) {
				tableDuration = dur
				break
			}
		}

		if tableDuration > 0 {
			elapsedSec := tableDuration.Seconds() - float64(expSec)
			addedAt := time.Now().Add(-time.Duration(elapsedSec) * time.Second)
			_, _ = fmt.Fprintf(w, "  - %s on %s (status: %s, duration: %v, expires in: %s, added: %s)\n",
				tableName, backend, status, tableDuration, formatDuration(expiresDuration), addedAt.Format("2006-01-02 15:04:05"))
		} else {
			_, _ = fmt.Fprintf(w, "  - %s on %s (status: %s, expires in: %s)\n",
				tableName, backend, status, formatDuration(expiresDuration))
		}
	}
}

// formatPersistenceState displays persistence state information if available
func formatPersistenceState(w http.ResponseWriter, persistState interface{}, hasPersist bool) {
	if !hasPersist {
		return
	}
	stateVal := reflect.ValueOf(persistState)
	state := stateVal.FieldByName("State").String()
	expireTime := stateVal.FieldByName("ExpireTime").Interface().(time.Time)
	reason := stateVal.FieldByName("Reason").String()

	_, _ = fmt.Fprintf(w, "persistence: %s\n", state)
	if !expireTime.IsZero() {
		_, _ = fmt.Fprintf(w, "persistence_expires: %s\n", expireTime.Format(time.RFC3339))
	}
	if reason != "" {
		_, _ = fmt.Fprintf(w, "persistence_reason: %s\n", reason)
	}
}

// aggregateActorStatus combines multiple actor activities into a status string
func aggregateActorStatus(actors []*store.ActorActivity, persistState interface{}, hasPersist bool) string {
	var result string

	// Extract ModifiedAt from persistence if available
	var persistModifiedAt time.Time
	if hasPersist {
		stateVal := reflect.ValueOf(persistState)
		modifiedAt := stateVal.FieldByName("ModifiedAt").Interface().(time.Time)
		if !modifiedAt.IsZero() {
			persistModifiedAt = modifiedAt
		}
	}

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

			// Use ModifiedAt from persistence if available, otherwise estimate
			var blockTime time.Time
			if !persistModifiedAt.IsZero() {
				blockTime = persistModifiedAt
			} else {
				blockTime = a.BlockedUntil.Add(-1 * time.Hour) // Fallback estimate
			}

			if earliestBlock.IsZero() || blockTime.Before(earliestBlock) {
				earliestBlock = blockTime
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
// IPStatusResponse is the JSON response for IP status queries
type IPStatusResponse struct {
	Node               string            `json:"node,omitempty"`
	Status             string            `json:"status"` // "blocked", "unblocked", "unknown"
	Actors             int               `json:"actors,omitempty"`
	Chains             map[string]string `json:"chains,omitempty"`         // chain -> expiry time (RFC3339)
	EarliestBlock      string            `json:"earliest_block,omitempty"` // RFC3339
	LatestExpiry       string            `json:"latest_expiry,omitempty"`  // RFC3339
	LastSeen           string            `json:"last_seen,omitempty"`      // RFC3339
	LastUnblock        string            `json:"last_unblock,omitempty"`   // RFC3339
	UnblockReason      string            `json:"unblock_reason,omitempty"`
	Backend            string            `json:"backend,omitempty"` // "present" if in HAProxy tables
	Persistence        string            `json:"persistence,omitempty"`
	PersistenceExpires string            `json:"persistence_expires,omitempty"`
	PersistenceReason  string            `json:"persistence_reason,omitempty"`
}

// apiIPLookupHandler returns IP status as JSON (cluster-aware)
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

		// If follower, forward to leader for cluster-wide view
		if p.GetNodeRole() == "follower" {
			path := fmt.Sprintf("/api/v1/ip/%s", canonical)
			resp, err := forwardToLeader(p, "GET", path, nil)
			if err != nil {
				p.Log(logging.LevelError, "API", "Failed to forward to leader: %v", err)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = fmt.Fprintf(w, `{"error":"Failed to contact leader: %s"}`, err.Error())
				return
			}
			defer func() { _ = resp.Body.Close() }()

			// Copy response from leader
			w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			return
		}

		// Leader or standalone: return cluster-wide or local view
		if p.GetNodeRole() == "leader" {
			// Return cluster-wide aggregated view
			response := queryAllNodes(p, canonical)
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(response); err != nil {
				p.Log(logging.LevelError, "API", "Failed to encode JSON response: %v", err)
			}
			return
		}

		// Standalone: return local view
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

	// Check persistence state
	var persistModifiedAt time.Time
	if persistState, hasPersist := p.GetPersistenceState(ip); hasPersist {
		stateVal := reflect.ValueOf(persistState)
		response.Persistence = stateVal.FieldByName("State").String()
		expireTime := stateVal.FieldByName("ExpireTime").Interface().(time.Time)
		if !expireTime.IsZero() {
			response.PersistenceExpires = expireTime.Format(time.RFC3339)
		}
		reason := stateVal.FieldByName("Reason").String()
		if reason != "" {
			response.PersistenceReason = reason
		}
		// Get ModifiedAt for accurate block time
		modifiedAt := stateVal.FieldByName("ModifiedAt").Interface().(time.Time)
		if !modifiedAt.IsZero() {
			persistModifiedAt = modifiedAt
		}
	}

	if len(actors) == 0 && !inBackend {
		response.Status = "unknown"
		return response
	}

	// If in HAProxy but not in activity store
	if inBackend && len(actors) == 0 {
		response.Status = "blocked"
		response.Backend = "present"
		// Note: PersistenceReason already set above if available
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

			// Use ModifiedAt from persistence if available, otherwise estimate
			var blockTime time.Time
			if !persistModifiedAt.IsZero() {
				blockTime = persistModifiedAt
			} else {
				blockTime = a.BlockedUntil.Add(-1 * time.Hour) // Fallback estimate
			}

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

	return response
}

// parseRFC3339 is a helper to parse RFC3339 time strings
func parseRFC3339(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// clearIPHandler unblocks an IP by removing it from all HAProxy tables on all backends
func clearIPHandler(p Provider) http.HandlerFunc {
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

		// If follower, forward to leader
		if p.GetNodeRole() == "follower" {
			path := fmt.Sprintf("/ip/%s/clear", canonical)
			resp, err := forwardToLeader(p, "DELETE", path, nil)
			if err != nil {
				p.Log(logging.LevelError, "API", "Failed to forward clear request to leader: %v", err)
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer func() { _ = resp.Body.Close() }()

			// Copy response from leader
			w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			return
		}

		// Leader: clear locally and broadcast to followers
		blockerInterface := p.GetBlocker()
		if blockerInterface == nil {
			http.Error(w, "Blocker not available", http.StatusServiceUnavailable)
			return
		}

		// Type assert to blocker.Blocker interface
		blocker, ok := blockerInterface.(blocker.Blocker)
		if !ok {
			http.Error(w, "Blocker interface error", http.StatusInternalServerError)
			return
		}

		// Create IPInfo using helper
		ipInfo := utils.NewIPInfo(canonical)

		// Clear the IP from all tables
		foundInfo, err := blocker.ClearIP(ipInfo)
		if err != nil {
			p.Log(logging.LevelError, "API", "Failed to clear IP %s: %v", canonical, err)
			http.Error(w, fmt.Sprintf("Failed to clear IP: %v", err), http.StatusInternalServerError)
			return
		}

		// Remove from persistence state
		if err := p.RemoveFromPersistence(canonical); err != nil {
			p.Log(logging.LevelError, "API", "Failed to remove IP %s from persistence: %v", canonical, err)
			// Don't fail the request, just log the error
		}

		// Clear from ActivityStore (in-memory chain progress)
		clearIPFromActivityStore(p, canonical)

		// Broadcast to followers (async)
		if p.GetNodeRole() == "leader" {
			go broadcastClearToFollowers(p, canonical)
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

		// Build response (without node name for internal use)
		response := buildIPStatusResponse(p, actors, canonical, inBackend)
		response.Node = "" // Leader will add node name

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
	Name               string            `json:"name"`
	Status             string            `json:"status"` // "blocked", "unblocked", "unknown", "error"
	Error              string            `json:"error,omitempty"`
	Actors             int               `json:"actors,omitempty"`
	Chains             map[string]string `json:"chains,omitempty"`
	EarliestBlock      string            `json:"earliest_block,omitempty"`
	LatestExpiry       string            `json:"latest_expiry,omitempty"`
	LastSeen           string            `json:"last_seen,omitempty"`
	LastUnblock        string            `json:"last_unblock,omitempty"`
	UnblockReason      string            `json:"unblock_reason,omitempty"`
	Persistence        string            `json:"persistence,omitempty"`
	PersistenceExpires string            `json:"persistence_expires,omitempty"`
	PersistenceReason  string            `json:"persistence_reason,omitempty"`
}

// NodeInfo is a minimal representation of cluster node to avoid import cycles
type NodeInfo struct {
	Name    string
	Address string
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

// internalClearIPHandler is the internal cluster endpoint for clearing IPs
// Called by leader to clear IP on follower nodes
func internalClearIPHandler(p Provider) http.HandlerFunc {
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

		// Followers only clear local persistence state
		// HAProxy instances are shared and already cleared by the leader
		if err := p.RemoveFromPersistence(canonical); err != nil {
			p.Log(logging.LevelError, "CLUSTER_CLEAR", "Failed to remove IP %s from persistence: %v", canonical, err)
			http.Error(w, fmt.Sprintf("Failed to remove from persistence: %v", err), http.StatusInternalServerError)
			return
		}

		// Clear from ActivityStore (in-memory chain progress)
		clearIPFromActivityStore(p, canonical)

		p.Log(logging.LevelInfo, "CLUSTER_CLEAR", "IP %s cleared from local persistence", canonical)
		w.WriteHeader(http.StatusOK)
	}
}

// broadcastClearToFollowers sends clear request to all follower nodes
func broadcastClearToFollowers(p Provider, ip string) {
	path := fmt.Sprintf("/api/v1/cluster/internal/ip/%s/clear", ip)
	broadcastToFollowers(p, "DELETE", path, nil)
}

// clearIPFromActivityStore removes all actors with the given IP from ActivityStore
func clearIPFromActivityStore(p Provider, ip string) {
	p.GetActivityMutex().Lock()
	defer p.GetActivityMutex().Unlock()

	activityStore := p.GetActivityStore()
	for actor := range activityStore {
		if actor.IPInfo.Address == ip {
			delete(activityStore, actor)
		}
	}
}

// forwardToLeader forwards an HTTP request to the leader node and returns the response
func forwardToLeader(p Provider, method, path string, body io.Reader) (*http.Response, error) {
	leaderAddr := p.GetNodeLeaderAddress()
	if leaderAddr == "" {
		return nil, fmt.Errorf("leader address not configured")
	}

	protocol := p.GetClusterProtocol()
	url := fmt.Sprintf("%s://%s%s", protocol, leaderAddr, path)

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to contact leader: %w", err)
	}

	return resp, nil
}

// broadcastToFollowers sends an HTTP request to all follower nodes asynchronously
func broadcastToFollowers(p Provider, method, path string, body io.Reader) {
	nodes := extractNodeInfo(p.GetClusterNodes())
	if nodes == nil {
		return
	}

	protocol := p.GetClusterProtocol()
	nodeName := p.GetNodeName()

	for _, node := range nodes {
		if node.Name == nodeName {
			continue // Skip self
		}

		go func(nodeName, nodeAddr string) {
			url := fmt.Sprintf("%s://%s%s", protocol, nodeAddr, path)
			client := &http.Client{Timeout: 5 * time.Second}

			req, err := http.NewRequest(method, url, body)
			if err != nil {
				p.Log(logging.LevelWarning, "CLUSTER_BROADCAST", "Failed to create request for node %s: %v", nodeName, err)
				return
			}

			resp, err := client.Do(req)
			if err != nil {
				p.Log(logging.LevelWarning, "CLUSTER_BROADCAST", "Failed to send %s %s to node %s: %v", method, path, nodeName, err)
				return
			}
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				p.Log(logging.LevelWarning, "CLUSTER_BROADCAST", "Node %s returned status %d for %s %s", nodeName, resp.StatusCode, method, path)
			} else {
				p.Log(logging.LevelDebug, "CLUSTER_BROADCAST", "Successfully sent %s %s to node %s", method, path, nodeName)
			}
		}(node.Name, node.Address)
	}
}
