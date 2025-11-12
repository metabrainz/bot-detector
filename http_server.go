package main

import (
	"bot-detector/internal/logging"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// startMetricsServer starts a simple HTTP server to expose metrics on a webpage.
// It should be run in a goroutine.
func startMetricsServer(p *Processor) {
	if p.Config.HTTPListenAddr == "" {
		p.LogFunc(logging.LevelDebug, "HTTP_SERVER", "HTTP server is disabled (http_listen_addr is not set).")
		return
	}

	mux := http.NewServeMux()
	// Pass the processor to the handler.
	mux.HandleFunc("/", metricsPageHandler(p))

	p.LogFunc(logging.LevelInfo, "HTTP_SERVER", "Starting metrics web server on http://%s", p.Config.HTTPListenAddr)

	server := &http.Server{
		Addr:    p.Config.HTTPListenAddr,
		Handler: mux,
	}

	// Listen for shutdown signal to gracefully close the server.
	go func() {
		<-p.signalCh
		p.LogFunc(logging.LevelInfo, "HTTP_SERVER", "Shutting down metrics web server.")
		server.Close()
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		p.LogFunc(logging.LevelCritical, "HTTP_SERVER_FATAL", "Metrics web server failed: %v", err)
	}
}

// metricsPageHandler creates an HTTP handler that displays the current metrics.
func metricsPageHandler(p *Processor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var report strings.Builder

		// Create a log function that writes to our string builder instead of the console.
		webLogFunc := func(level logging.LogLevel, tag string, format string, args ...interface{}) {
			// We can ignore level and tag for web output.
			report.WriteString(fmt.Sprintf(format, args...))
			report.WriteString("\n")
		}

		// Calculate elapsed time since the processor started.
		elapsedTime := time.Since(p.startTime)

		// Generate the metrics summary into the string builder.
		// The "metric" filter tag ensures all relevant metrics are shown.
		logMetricsSummary(p, elapsedTime, webLogFunc, "METRICS", "metric")

		// Format the output as a simple, pre-formatted HTML page.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!DOCTYPE html>
<html>
<head>
<title>Bot-Detector Metrics</title>
<meta http-equiv="refresh" content="5">
<style>body { font-family: monospace; background-color: #f4f4f4; color: #333; }</style>
</head>
<body>
<pre>`)
		io.WriteString(w, report.String())
		io.WriteString(w, `</pre>
</body>
</html>`)
	}
}
