package server

import (
	"fmt"
	"net/http"
	"strings"

	"bot-detector/internal/logging"
)

// rootHandler returns an HTTP handler that serves the plain-text metrics dashboard.
// This handler is registered for both "/" and "/stats" endpoints.
func rootHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, err := fmt.Fprint(w, p.GenerateMetricsReport())
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

// websitesHandler returns an HTTP handler that serves multi-website statistics.
// This handler is registered for the "/stats/websites" endpoint.
func websitesHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, err := fmt.Fprint(w, p.GenerateWebsiteStatsReport())
		if err != nil {
			logging.LogOutput(logging.LevelError, "websitesHandler", "Error writing website stats: %v", err)
		}
	}
}

// parseErrorsHandler returns an HTTP handler that serves recent parse error log lines.
// This handler is registered for the "/stats/parse-errors" endpoint.
func parseErrorsHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		entries := p.GetRecentParseErrors()
		if len(entries) == 0 {
			_, _ = fmt.Fprint(w, "No recent parse errors.\n")
			return
		}
		_, err := fmt.Fprint(w, strings.Join(entries, "\n")+"\n")
		if err != nil {
			logging.LogOutput(logging.LevelError, "parseErrorsHandler", "Error writing parse errors: %v", err)
		}
	}
}
