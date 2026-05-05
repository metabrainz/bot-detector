package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"bot-detector/internal/logging"
)

// GET /api/v2/challenge/{website}/{ip}
func challengeStatusHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		website := r.PathValue("website")
		ip := r.PathValue("ip")
		if website == "" || ip == "" {
			jsonError(w, "website and ip required", http.StatusBadRequest)
			return
		}

		challenged, reason, err := p.GetChallengeStatus(ip, website)
		if err != nil {
			jsonError(w, fmt.Sprintf("failed to check challenge: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ip":         ip,
			"website":    website,
			"challenged": challenged,
			"reason":     reason,
		})
	}
}

// POST /api/v2/challenge/{website}/{ip}
func challengeIPHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		website := r.PathValue("website")
		ip := r.PathValue("ip")
		if website == "" || ip == "" {
			jsonError(w, "website and ip required", http.StatusBadRequest)
			return
		}

		// Parse optional duration from query param (default 24h)
		duration := 24 * time.Hour
		if d := r.URL.Query().Get("duration"); d != "" {
			parsed, err := time.ParseDuration(d)
			if err != nil {
				jsonError(w, fmt.Sprintf("invalid duration: %v", err), http.StatusBadRequest)
				return
			}
			duration = parsed
		}

		reason := r.URL.Query().Get("reason")
		if reason == "" {
			reason = "manual"
		}

		if err := p.ChallengeIP(ip, website, duration, reason); err != nil {
			jsonError(w, fmt.Sprintf("failed to challenge: %v", err), http.StatusInternalServerError)
			return
		}

		p.Log(logging.LevelInfo, "API", "Manually challenged %s on %s for %v (reason: %s)", ip, website, duration, reason)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ip":       ip,
			"website":  website,
			"duration": duration.String(),
			"reason":   reason,
			"status":   "challenged",
		})
	}
}

// DELETE /api/v2/challenge/{website}/{ip}
func unchallengeIPHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		website := r.PathValue("website")
		ip := r.PathValue("ip")
		if website == "" || ip == "" {
			jsonError(w, "website and ip required", http.StatusBadRequest)
			return
		}

		if err := p.UnchallengeIP(ip, website); err != nil {
			jsonError(w, fmt.Sprintf("failed to unchallenge: %v", err), http.StatusInternalServerError)
			return
		}

		p.Log(logging.LevelInfo, "API", "Removed challenge for %s on %s", ip, website)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ip":      ip,
			"website": website,
			"status":  "unchallenged",
		})
	}
}
