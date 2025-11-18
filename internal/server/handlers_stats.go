package server

import (
	"fmt"
	"net/http"

	"bot-detector/internal/logging"
)

// rootHandler returns an HTTP handler that serves the HTML metrics dashboard.
// This handler is registered for both "/" and "/stats" endpoints.
func rootHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, err := fmt.Fprint(w, p.GenerateHTMLMetricsReport())
		if err != nil {
			logging.LogOutput(logging.LevelError, "metricsHandler", "Error writing metrics report: %v", err)
		}
	}
}

// stepsHandler returns an HTTP handler that serves plain-text step execution metrics.
// This handler is registered for the "/stats/steps" endpoint.
func stepsHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, err := fmt.Fprint(w, p.GenerateStepsMetricsReport())
		if err != nil {
			logging.LogOutput(logging.LevelError, "stepsHandler", "Error writing steps report: %v", err)
		}
	}
}
