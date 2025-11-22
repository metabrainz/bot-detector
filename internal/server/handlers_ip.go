package server

import (
	"fmt"
	"net"
	"net/http"
	"time"

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
