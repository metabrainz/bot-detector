package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"bot-detector/internal/logging"
	"bot-detector/internal/store"
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
		defer p.GetActivityMutex().RUnlock()

		activityStore := p.GetActivityStore()

		// Collect all actors with this IP
		var actors []*store.ActorActivity
		for actor, activity := range activityStore {
			if actor.IPInfo.Address == canonical {
				actors = append(actors, activity)
			}
		}

		// Format response
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		nodeName := p.GetNodeName()
		if nodeName != "" {
			_, _ = fmt.Fprintf(w, "node: %s\n", nodeName)
		}

		if len(actors) == 0 {
			_, _ = fmt.Fprint(w, "status: unknown\n")
			addFollowerHint(w, p, canonical)
			return
		}

		// Aggregate status
		status := aggregateActorStatus(actors)
		_, _ = fmt.Fprint(w, status)
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
		defer p.GetActivityMutex().RUnlock()

		activityStore := p.GetActivityStore()

		// Collect all actors with this IP
		var actors []*store.ActorActivity
		for actor, activity := range activityStore {
			if actor.IPInfo.Address == canonical {
				actors = append(actors, activity)
			}
		}

		// Build response
		response := buildIPStatusResponse(p, actors, canonical)

		// Return JSON
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			p.Log(logging.LevelError, "API", "Failed to encode JSON response: %v", err)
		}
	}
}

// buildIPStatusResponse creates JSON response from actor activities
func buildIPStatusResponse(p Provider, actors []*store.ActorActivity, ip string) IPStatusResponse {
	response := IPStatusResponse{
		Node: p.GetNodeName(),
	}

	if len(actors) == 0 {
		response.Status = "unknown"
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
