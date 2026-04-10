package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"bot-detector/internal/logging"
	"bot-detector/internal/persistence"
)

// badActorsListHandler returns all bad actors as JSON.
// GET /api/v1/bad-actors
func badActorsListHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actors, err := p.GetAllBadActors()
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		type entry struct {
			IP          string  `json:"ip"`
			PromotedAt  string  `json:"promoted_at"`
			TotalScore  float64 `json:"total_score"`
			BlockCount  int     `json:"block_count"`
			HistoryJSON string  `json:"history,omitempty"`
		}

		var result []entry
		for _, a := range actors {
			if ba, ok := a.(persistence.BadActorInfo); ok {
				result = append(result, entry{
					IP:          ba.IP,
					PromotedAt:  ba.PromotedAt.Format("2006-01-02T15:04:05Z"),
					TotalScore:  ba.TotalScore,
					BlockCount:  ba.BlockCount,
					HistoryJSON: ba.HistoryJSON,
				})
			}
		}
		if result == nil {
			result = []entry{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result) //nolint:errcheck
	}
}

// badActorsExportHandler returns all bad actor IPs as plain text, one per line.
// GET /api/v1/bad-actors/export
func badActorsExportHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actors, err := p.GetAllBadActors()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		for _, a := range actors {
			if ba, ok := a.(persistence.BadActorInfo); ok {
				fmt.Fprintln(w, ba.IP) //nolint:errcheck
			}
		}
	}
}

// badActorsDeleteByReasonHandler removes bad actors whose history contains the given reason.
// DELETE /api/v1/bad-actors?reason=chainName&unblock
// Cluster-aware: followers forward to leader, leader broadcasts to followers.
func badActorsDeleteByReasonHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reason := r.URL.Query().Get("reason")
		if reason == "" {
			jsonError(w, "reason query parameter is required", http.StatusBadRequest)
			return
		}

		// Follower: forward to leader
		if p.GetNodeRole() == "follower" {
			resp, err := forwardToLeader(p, "DELETE", r.URL.RequestURI(), nil)
			if err != nil {
				p.Log(logging.LevelError, "API", "Failed to forward bad-actors delete to leader: %v", err)
				jsonError(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			return
		}

		// Leader (or standalone): remove locally
		removed, err := p.RemoveBadActorsByReason(reason)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Broadcast removal to followers
		if p.GetNodeRole() == "leader" {
			go broadcastToFollowers(p, "DELETE",
				fmt.Sprintf("/api/v1/cluster/internal/bad-actors?reason=%s", url.QueryEscape(reason)), nil)
		}

		// If &unblock is present, also unblock the removed IPs from HAProxy
		var unblocked []string
		var unblockErrors []string
		if _, ok := r.URL.Query()["unblock"]; ok {
			for _, ip := range removed {
				if err := unblockIP(p, ip); err != nil {
					p.Log(logging.LevelError, "API", "Failed to unblock bad actor %s: %v", ip, err)
					unblockErrors = append(unblockErrors, ip)
				} else {
					unblocked = append(unblocked, ip)
				}
			}
		}

		resp := map[string]interface{}{
			"reason":  reason,
			"removed": len(removed),
			"ips":     removed,
		}
		if _, ok := r.URL.Query()["unblock"]; ok {
			resp["unblocked"] = len(unblocked)
			if len(unblockErrors) > 0 {
				resp["unblock_errors"] = unblockErrors
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}
}

// internalBadActorsDeleteHandler handles cluster-internal bad actor removal on followers.
// DELETE /api/v1/cluster/internal/bad-actors?reason=chainName
func internalBadActorsDeleteHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reason := r.URL.Query().Get("reason")
		if reason == "" {
			http.Error(w, "reason query parameter is required", http.StatusBadRequest)
			return
		}

		removed, err := p.RemoveBadActorsByReason(reason)
		if err != nil {
			p.Log(logging.LevelError, "CLUSTER_BAD_ACTORS", "Failed to remove bad actors by reason %q: %v", reason, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		p.Log(logging.LevelInfo, "CLUSTER_BAD_ACTORS", "Removed %d bad actors by reason %q", len(removed), reason)
		w.WriteHeader(http.StatusOK)
	}
}

// badActorsStatsHandler returns aggregated statistics about bad actors.
// GET /api/v1/bad-actors/stats
func badActorsStatsHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actors, err := p.GetAllBadActors()
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		type histEntry struct {
			Reason string `json:"r"`
		}

		byReason := make(map[string]int)
		byDay := make(map[string]int)
		var totalScore float64
		var totalBlocks int

		for _, a := range actors {
			ba, ok := a.(persistence.BadActorInfo)
			if !ok {
				continue
			}
			totalScore += ba.TotalScore
			totalBlocks += ba.BlockCount
			byDay[ba.PromotedAt.Format("2006-01-02")]++

			if ba.HistoryJSON == "" {
				continue
			}
			var history []histEntry
			if err := json.Unmarshal([]byte(ba.HistoryJSON), &history); err != nil {
				continue
			}
			seen := make(map[string]bool)
			for _, h := range history {
				if h.Reason != "" && !seen[h.Reason] {
					seen[h.Reason] = true
					byReason[h.Reason]++
				}
			}
		}

		total := len(actors)
		var avgScore float64
		var avgBlocks float64
		if total > 0 {
			avgScore = totalScore / float64(total)
			avgBlocks = float64(totalBlocks) / float64(total)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"total":           total,
			"avg_score":       avgScore,
			"avg_block_count": avgBlocks,
			"by_reason":       byReason,
			"by_day":          byDay,
		})
	}
}
