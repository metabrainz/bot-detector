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
	"bot-detector/internal/persistence"
	"bot-detector/internal/store"
	"bot-detector/internal/utils"
)

// ipLookupHandler returns IP status in plain text (cluster-aware)
func ipLookupHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ipStr := r.PathValue("ip")
		if ipStr == "" {
			jsonError(w, "IP address required", http.StatusBadRequest)
			return
		}

		// Canonicalize IP
		ip := net.ParseIP(ipStr)
		if ip == nil {
			jsonError(w, "Invalid IP address", http.StatusBadRequest)
			return
		}
		canonical := ip.String()

		// If follower, forward to leader for cluster-wide view
		if p.GetNodeRole() == "follower" {
			path := fmt.Sprintf("/ip/%s", canonical)
			resp, err := forwardToLeader(p, "GET", path, nil)
			if err != nil {
				p.Log(logging.LevelError, "IP_LOOKUP", "Failed to forward to leader: %v", err)
				jsonError(w, err.Error(), http.StatusBadGateway)
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
		formatPersistenceState(w, p, canonical, persistState, hasPersist)
		return
	}

	// If in HAProxy but not in activity store, show backend details
	if len(backendInfo) > 0 && len(actors) == 0 {
		_, _ = fmt.Fprint(w, "status: blocked\n")
		_, _ = fmt.Fprint(w, "source: backend\n")
		formatBackendInfo(w, backendInfo, p.GetDurationTables())
		formatPersistenceState(w, p, canonical, persistState, hasPersist)
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

	formatPersistenceState(w, p, canonical, persistState, hasPersist)
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
		if node.Address != "" {
			_, _ = fmt.Fprintf(w, "  - name: %s (%s)\n", node.Name, node.Address)
		} else {
			_, _ = fmt.Fprintf(w, "  - name: %s\n", node.Name)
		}
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
		if node.BadActor != nil {
			_, _ = fmt.Fprintf(w, "    bad_actor: yes\n")
			_, _ = fmt.Fprintf(w, "    bad_actor_promoted_at: %s\n", node.BadActor.PromotedAt)
			_, _ = fmt.Fprintf(w, "    bad_actor_score: %.1f\n", node.BadActor.TotalScore)
			_, _ = fmt.Fprintf(w, "    bad_actor_block_count: %d\n", node.BadActor.BlockCount)
			formatBadActorHistory(w, node.BadActor.History, "    ")
		} else if node.Score != nil {
			_, _ = fmt.Fprintf(w, "    score: %.1f / %.1f\n", node.Score.CurrentScore, node.Score.Threshold)
			_, _ = fmt.Fprintf(w, "    score_block_count: %d\n", node.Score.BlockCount)
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
								tableName, backend, status, tableDuration, formatDuration(expiresDuration), addedAt.Format(time.RFC3339))
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
				tableName, backend, status, tableDuration, formatDuration(expiresDuration), addedAt.Format(time.RFC3339))
		} else {
			_, _ = fmt.Fprintf(w, "  - %s on %s (status: %s, expires in: %s)\n",
				tableName, backend, status, formatDuration(expiresDuration))
		}
	}
}

// formatPersistenceState displays persistence state information if available

func formatBadActorHistory(w http.ResponseWriter, historyJSON string, indent string) {
	if historyJSON == "" {
		return
	}
	var history []struct {
		Timestamp string `json:"ts"`
		Reason    string `json:"r"`
	}
	if err := json.Unmarshal([]byte(historyJSON), &history); err != nil || len(history) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "%sbad_actor_history:\n", indent)
	for _, h := range history {
		_, _ = fmt.Fprintf(w, "%s  - %s %s\n", indent, h.Timestamp, h.Reason)
	}
}
func formatPersistenceState(w http.ResponseWriter, p Provider, canonical string, persistState interface{}, hasPersist bool) {
	if hasPersist {
		stateVal := reflect.ValueOf(persistState)
		state := fmt.Sprintf("%s", stateVal.FieldByName("State").Interface())
		expireTime := stateVal.FieldByName("ExpireTime").Interface().(time.Time)
		reason := stateVal.FieldByName("Reason").String()
		modifiedAt := stateVal.FieldByName("ModifiedAt").Interface().(time.Time)
		firstBlockedAt := stateVal.FieldByName("FirstBlockedAt").Interface().(time.Time)

		_, _ = fmt.Fprintf(w, "persistence: %s\n", state)
		if !expireTime.IsZero() {
			_, _ = fmt.Fprintf(w, "persistence_expires: %s\n", expireTime.Format(time.RFC3339))
		}
		if reason != "" {
			_, _ = fmt.Fprintf(w, "persistence_reason: %s\n", reason)
		}
		if !firstBlockedAt.IsZero() {
			_, _ = fmt.Fprintf(w, "persistence_first_blocked: %s\n", firstBlockedAt.Format(time.RFC3339))
		}
		if !modifiedAt.IsZero() {
			_, _ = fmt.Fprintf(w, "persistence_modified: %s\n", modifiedAt.Format(time.RFC3339))
		}
	}

	// Bad actor / score info
	baInfo, scoreInfo := p.GetBadActorInfo(canonical)
	if baInfo != nil {
		if ba, ok := baInfo.(*persistence.BadActorInfo); ok && ba != nil {
			_, _ = fmt.Fprintf(w, "bad_actor: yes\n")
			_, _ = fmt.Fprintf(w, "bad_actor_promoted_at: %s\n", ba.PromotedAt.Format(time.RFC3339))
			_, _ = fmt.Fprintf(w, "bad_actor_score: %.1f\n", ba.TotalScore)
			_, _ = fmt.Fprintf(w, "bad_actor_block_count: %d\n", ba.BlockCount)
			formatBadActorHistory(w, ba.HistoryJSON, "")
		}
	} else if scoreInfo != nil {
		if s, ok := scoreInfo.(*persistence.ScoreInfo); ok && s != nil {
			threshold := p.GetBadActorsThreshold()
			if threshold > 0 {
				_, _ = fmt.Fprintf(w, "score: %.1f / %.1f\n", s.Score, threshold)
				_, _ = fmt.Fprintf(w, "score_block_count: %d\n", s.BlockCount)
			}
		}
	}
}

// aggregateActorStatus combines multiple actor activities into a status string
func aggregateActorStatus(actors []*store.ActorActivity, persistState interface{}, hasPersist bool) string {
	var result string

	// Extract FirstBlockedAt from persistence if available
	var persistFirstBlockedAt time.Time
	if hasPersist {
		stateVal := reflect.ValueOf(persistState)
		firstBlockedAt := stateVal.FieldByName("FirstBlockedAt").Interface().(time.Time)
		if !firstBlockedAt.IsZero() {
			persistFirstBlockedAt = firstBlockedAt
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

			// Use FirstBlockedAt from persistence (cluster-wide earliest), fallback to estimate
			var blockTime time.Time
			if !persistFirstBlockedAt.IsZero() {
				blockTime = persistFirstBlockedAt
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
	BadActor           *BadActorStatus   `json:"bad_actor,omitempty"`
	Score              *ScoreStatus      `json:"score,omitempty"`
}

// BadActorStatus is included in IP lookup when the IP is a bad actor.
type BadActorStatus struct {
	PromotedAt string  `json:"promoted_at"`
	TotalScore float64 `json:"total_score"`
	BlockCount int     `json:"block_count"`
	History    string  `json:"history,omitempty"`
}

// ScoreStatus is included in IP lookup when the IP has a score but is not yet a bad actor.
type ScoreStatus struct {
	CurrentScore float64 `json:"current_score"`
	BlockCount   int     `json:"block_count"`
	Threshold    float64 `json:"threshold"`
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
	var persistFirstBlockedAt time.Time
	if persistState, hasPersist := p.GetPersistenceState(ip); hasPersist {
		stateVal := reflect.ValueOf(persistState)
		response.Persistence = fmt.Sprintf("%s", stateVal.FieldByName("State").Interface())
		expireTime := stateVal.FieldByName("ExpireTime").Interface().(time.Time)
		if !expireTime.IsZero() {
			response.PersistenceExpires = expireTime.Format(time.RFC3339)
		}
		reason := stateVal.FieldByName("Reason").String()
		if reason != "" {
			response.PersistenceReason = reason
		}
		// Get FirstBlockedAt for cluster-wide earliest block time
		firstBlockedAt := stateVal.FieldByName("FirstBlockedAt").Interface().(time.Time)
		if !firstBlockedAt.IsZero() {
			persistFirstBlockedAt = firstBlockedAt
		}
	}

	if len(actors) == 0 && !inBackend {
		response.Status = "unknown"
		populateBadActorInfo(p, &response, ip)
		return response
	}

	// If in HAProxy but not in activity store
	if inBackend && len(actors) == 0 {
		response.Status = "blocked"
		response.Backend = "present"
		if !persistFirstBlockedAt.IsZero() {
			response.EarliestBlock = persistFirstBlockedAt.Format(time.RFC3339)
		}
		populateBadActorInfo(p, &response, ip)
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

			// Use FirstBlockedAt from persistence (cluster-wide earliest), fallback to estimate
			var blockTime time.Time
			if !persistFirstBlockedAt.IsZero() {
				blockTime = persistFirstBlockedAt
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

	// Add bad actor / score info
	populateBadActorInfo(p, &response, ip)

	return response
}

// populateBadActorInfo adds bad actor and score sections to the response.
func populateBadActorInfo(p Provider, response *IPStatusResponse, ip string) {
	baInfo, scoreInfo := p.GetBadActorInfo(ip)
	if baInfo != nil {
		if ba, ok := baInfo.(*persistence.BadActorInfo); ok && ba != nil {
			response.BadActor = &BadActorStatus{
				PromotedAt: ba.PromotedAt.Format(time.RFC3339),
				TotalScore: ba.TotalScore,
				BlockCount: ba.BlockCount,
				History:    ba.HistoryJSON,
			}
		}
	}
	if scoreInfo != nil && response.BadActor == nil {
		// Only show score if NOT already a bad actor (score is redundant once promoted)
		if s, ok := scoreInfo.(*persistence.ScoreInfo); ok && s != nil {
			threshold := p.GetBadActorsThreshold()
			if threshold > 0 {
				response.Score = &ScoreStatus{
					CurrentScore: s.Score,
					BlockCount:   s.BlockCount,
					Threshold:    threshold,
				}
			}
		}
	}
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
			jsonError(w, "IP address required", http.StatusBadRequest)
			return
		}

		// Canonicalize IP
		ip := net.ParseIP(ipStr)
		if ip == nil {
			jsonError(w, "Invalid IP address", http.StatusBadRequest)
			return
		}
		canonical := ip.String()

		// If follower, forward to leader
		if p.GetNodeRole() == "follower" {
			path := fmt.Sprintf("/ip/%s/clear", canonical)
			resp, err := forwardToLeader(p, "DELETE", path, nil)
			if err != nil {
				p.Log(logging.LevelError, "API", "Failed to forward clear request to leader: %v", err)
				jsonError(w, err.Error(), http.StatusBadGateway)
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
			jsonError(w, "Blocker not available", http.StatusServiceUnavailable)
			return
		}

		// Type assert to blocker.Blocker interface
		blocker, ok := blockerInterface.(blocker.Blocker)
		if !ok {
			jsonError(w, "Blocker interface error", http.StatusInternalServerError)
			return
		}

		// Create IPInfo using helper
		ipInfo := utils.NewIPInfo(canonical)

		// Clear the IP from all tables
		foundInfo, err := blocker.ClearIP(ipInfo)
		if err != nil {
			p.Log(logging.LevelError, "API", "Failed to clear IP %s: %v", canonical, err)
			jsonError(w, fmt.Sprintf("Failed to clear IP: %v", err), http.StatusInternalServerError)
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
					tableName, backend, status, tableDuration, formatDuration(expiresDuration), addedAt.Format(time.RFC3339))
			} else {
				_, _ = fmt.Fprintf(w, "  - %s on %s (status: %s, expires in: %s)\n",
					tableName, backend, status, formatDuration(expiresDuration))
			}
		}

		p.Log(logging.LevelInfo, "API", "IP %s cleared from %d table entries", canonical, len(foundInfo))
	}
}

// unblockIPHandler unblocks an IP by setting gpc0=0 in HAProxy tables (fast, lets entry expire naturally)
func unblockIPHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ipStr := r.PathValue("ip")
		if ipStr == "" {
			jsonError(w, "IP address required", http.StatusBadRequest)
			return
		}

		// Canonicalize IP
		ip := net.ParseIP(ipStr)
		if ip == nil {
			jsonError(w, "Invalid IP address", http.StatusBadRequest)
			return
		}
		canonical := ip.String()

		// If follower, forward to leader
		if p.GetNodeRole() == "follower" {
			path := fmt.Sprintf("/ip/%s/unblock", canonical)
			resp, err := forwardToLeader(p, r.Method, path, nil)
			if err != nil {
				p.Log(logging.LevelError, "API", "Failed to forward unblock request to leader: %v", err)
				jsonError(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer func() { _ = resp.Body.Close() }()

			w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			return
		}

		// Leader: unblock locally and broadcast to followers
		if err := unblockIP(p, canonical); err != nil {
			p.Log(logging.LevelError, "API", "Failed to unblock IP %s: %v", canonical, err)
			jsonError(w, fmt.Sprintf("Failed to unblock IP: %v", err), http.StatusInternalServerError)
			return
		}

		// If GET request, show status after unblocking
		if r.Method == "GET" {
			// Small delay to let unblock propagate
			time.Sleep(100 * time.Millisecond)

			// Show cluster-wide status
			renderClusterIPStatus(w, p, canonical)
			return
		}

		// POST request: simple confirmation
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "IP %s unblocked (gpc0 set to 0, entry will expire naturally)\n", canonical)
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
	Address            string            `json:"address,omitempty"`
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
	BadActor           *BadActorStatus   `json:"bad_actor,omitempty"`
	Score              *ScoreStatus      `json:"score,omitempty"`
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
			Name:    nodeName,
			Address: nodeAddr,
			Status:  "error",
			Error:   err.Error(),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		p.Log(logging.LevelWarning, "CLUSTER", "Node %s returned status %d", nodeName, resp.StatusCode)
		return NodeIPStatusResponse{
			Name:    nodeName,
			Address: nodeAddr,
			Status:  "error",
			Error:   fmt.Sprintf("HTTP %d", resp.StatusCode),
		}
	}

	var status IPStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		p.Log(logging.LevelWarning, "CLUSTER", "Failed to decode response from node %s: %v", nodeName, err)
		return NodeIPStatusResponse{
			Name:    nodeName,
			Address: nodeAddr,
			Status:  "error",
			Error:   "decode error",
		}
	}

	// Convert to NodeIPStatusResponse
	return NodeIPStatusResponse{
		Name:               nodeName,
		Address:            nodeAddr,
		Status:             status.Status,
		Actors:             status.Actors,
		Chains:             status.Chains,
		EarliestBlock:      status.EarliestBlock,
		LatestExpiry:       status.LatestExpiry,
		LastSeen:           status.LastSeen,
		LastUnblock:        status.LastUnblock,
		UnblockReason:      status.UnblockReason,
		Persistence:        status.Persistence,
		PersistenceExpires: status.PersistenceExpires,
		PersistenceReason:  status.PersistenceReason,
		BadActor:           status.BadActor,
		Score:              status.Score,
	}
}

// internalClearIPHandler is the internal cluster endpoint for clearing IPs
// Called by leader to clear IP on follower nodes
func internalClearIPHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ipStr := r.PathValue("ip")
		if ipStr == "" {
			jsonError(w, "IP address required", http.StatusBadRequest)
			return
		}

		// Canonicalize IP
		ip := net.ParseIP(ipStr)
		if ip == nil {
			jsonError(w, "Invalid IP address", http.StatusBadRequest)
			return
		}
		canonical := ip.String()

		// Followers only clear local persistence state
		// HAProxy instances are shared and already cleared by the leader
		if err := p.RemoveFromPersistence(canonical); err != nil {
			p.Log(logging.LevelError, "CLUSTER_CLEAR", "Failed to remove IP %s from persistence: %v", canonical, err)
			jsonError(w, fmt.Sprintf("Failed to remove from persistence: %v", err), http.StatusInternalServerError)
			return
		}

		// Clear from ActivityStore (in-memory chain progress)
		clearIPFromActivityStore(p, canonical)

		p.Log(logging.LevelInfo, "CLUSTER_CLEAR", "IP %s cleared from local persistence", canonical)
		w.WriteHeader(http.StatusOK)
	}
}

func internalUnblockIPHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ipStr := r.PathValue("ip")
		if ipStr == "" {
			jsonError(w, "IP address required", http.StatusBadRequest)
			return
		}

		// Canonicalize IP
		ip := net.ParseIP(ipStr)
		if ip == nil {
			jsonError(w, "Invalid IP address", http.StatusBadRequest)
			return
		}
		canonical := ip.String()

		// Followers update local persistence state
		// HAProxy instances are shared and already unblocked by the leader
		if err := p.RemoveFromPersistence(canonical); err != nil {
			p.Log(logging.LevelError, "CLUSTER_UNBLOCK", "Failed to update persistence for IP %s: %v", canonical, err)
			jsonError(w, fmt.Sprintf("Failed to update persistence: %v", err), http.StatusInternalServerError)
			return
		}

		// Clear from ActivityStore
		clearIPFromActivityStore(p, canonical)

		p.Log(logging.LevelInfo, "CLUSTER_UNBLOCK", "IP %s unblocked in local persistence", canonical)
		w.WriteHeader(http.StatusOK)
	}
}

// broadcastClearToFollowers sends clear request to all follower nodes
func broadcastClearToFollowers(p Provider, ip string) {
	path := fmt.Sprintf("/api/v1/cluster/internal/ip/%s/clear", ip)
	broadcastToFollowers(p, "DELETE", path, nil)
}

func broadcastUnblockToFollowers(p Provider, ip string) {
	path := fmt.Sprintf("/api/v1/cluster/internal/ip/%s/unblock", ip)
	broadcastToFollowers(p, "POST", path, nil)
}

// clearIPFromActivityStore removes all actors with the given IP from ActivityStore
// unblockIP performs the core unblock logic for a single IP: HAProxy unblock, persistence removal,
// activity store cleanup, and follower broadcast. Returns an error if the HAProxy unblock fails.
func unblockIP(p Provider, canonical string) error {
	blockerInterface := p.GetBlocker()
	if blockerInterface == nil {
		return fmt.Errorf("blocker not available")
	}

	b, ok := blockerInterface.(blocker.Blocker)
	if !ok {
		return fmt.Errorf("blocker interface error")
	}

	ipInfo := utils.NewIPInfo(canonical)

	type directUnblocker interface {
		UnblockDirect(ipInfo utils.IPInfo, reason string) error
	}
	var unblockErr error
	if du, ok := blockerInterface.(directUnblocker); ok {
		unblockErr = du.UnblockDirect(ipInfo, "API unblock")
	} else {
		unblockErr = b.Unblock(ipInfo, "API unblock")
	}
	if unblockErr != nil {
		return unblockErr
	}

	if err := p.RemoveFromPersistence(canonical); err != nil {
		p.Log(logging.LevelError, "API", "Failed to update persistence for IP %s: %v", canonical, err)
	}

	clearIPFromActivityStore(p, canonical)

	if p.GetNodeRole() == "leader" {
		go broadcastUnblockToFollowers(p, canonical)
	}

	p.Log(logging.LevelInfo, "API", "IP %s unblocked via API", canonical)
	return nil
}

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
