package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"bot-detector/internal/persistence"
)

// badActorsListHandler returns all bad actors as JSON.
// GET /api/v1/bad-actors
func badActorsListHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actors, err := p.GetAllBadActors()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
		json.NewEncoder(w).Encode(result)
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
				fmt.Fprintln(w, ba.IP)
			}
		}
	}
}
