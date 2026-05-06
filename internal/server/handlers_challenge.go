package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"bot-detector/internal/logging"
)

// GET /api/v1/challenge/{ip}
func challengeStatusHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := r.PathValue("ip")
		if ip == "" {
			jsonError(w, "ip required", http.StatusBadRequest)
			return
		}

		challenged, err := p.GetChallengeStatus(ip)
		if err != nil {
			jsonError(w, fmt.Sprintf("failed to check challenge: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ip":         ip,
			"challenged": challenged,
		})
	}
}

// POST /api/v1/challenge/{ip}
func challengeIPHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := r.PathValue("ip")
		if ip == "" {
			jsonError(w, "ip required", http.StatusBadRequest)
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

		if err := p.ChallengeIP(ip, duration); err != nil {
			jsonError(w, fmt.Sprintf("failed to challenge: %v", err), http.StatusInternalServerError)
			return
		}

		p.Log(logging.LevelInfo, "API", "Manually challenged %s for %v", ip, duration)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ip":       ip,
			"duration": duration.String(),
			"status":   "challenged",
		})
	}
}

// DELETE /api/v1/challenge/{ip}
func unchallengeIPHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := r.PathValue("ip")
		if ip == "" {
			jsonError(w, "ip required", http.StatusBadRequest)
			return
		}

		if err := p.UnchallengeIP(ip); err != nil {
			jsonError(w, fmt.Sprintf("failed to unchallenge: %v", err), http.StatusInternalServerError)
			return
		}

		p.Log(logging.LevelInfo, "API", "Removed challenge for %s", ip)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ip":     ip,
			"status": "unchallenged",
		})
	}
}
