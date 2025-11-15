package server

import (
	"bot-detector/internal/logging"
	"bytes"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Provider defines the interface that the server needs from the main application.
type Provider interface {
	GetListenAddr() string
	GenerateHTMLMetricsReport() string
	GenerateStepsMetricsReport() string
	GetShutdownChannel() chan os.Signal
	Log(level logging.LogLevel, tag string, format string, v ...interface{})
	GetMarshalledConfig() ([]byte, time.Time, error)
}

// Start runs the web server in a goroutine.
func Start(p Provider) {
	listenAddr := p.GetListenAddr()
	if listenAddr == "" {
		p.Log(logging.LevelDebug, "HTTP_SERVER", "HTTP server is disabled.")
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/stats", statsPageHandler(p))
	mux.HandleFunc("/stats/steps", stepsStatsPageHandler(p))
	mux.HandleFunc("/config", configHandler(p)) // Register the new handler
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/stats", http.StatusFound) // 302 Found
	})

	p.Log(logging.LevelInfo, "HTTP_SERVER", "Starting web server on http://%s", listenAddr)

	server := &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}

	// Listen for shutdown signal to gracefully close the server.
	go func() {
		s := <-p.GetShutdownChannel()
		p.Log(logging.LevelInfo, "HTTP_SERVER", "Shutting down web server.")
		if err := server.Close(); err != nil {
			p.Log(logging.LevelError, "HTTP_SERVER", "Error closing web server: %v", err)
		}

		// Re-broadcast the signal so other listeners can also receive it.
		select {
		case p.GetShutdownChannel() <- s:
		default:
		}
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		p.Log(logging.LevelCritical, "HTTP_SERVER_FATAL", "Web server failed: %v", err)
	}
}

func servePage(w http.ResponseWriter, r *http.Request, title string, content string, name string) {
	// Format the output as a simple, pre-formatted HTML page.
	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<title>%s</title>
<meta http-equiv="refresh" content="5">
<style>body { font-family: monospace; background-color: #f4f4f4; color: #333; }</style>
</head>
<body><pre>%s</pre>
</body>
</html>`, title, content)
	http.ServeContent(w, r, name, time.Now(), bytes.NewReader([]byte(html)))
}

// stepsStatsPageHandler creates an HTTP handler that displays the step execution stats.
func stepsStatsPageHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reportContent := p.GenerateStepsMetricsReport()
		servePage(w, r, "Bot-Detector Step Stats", reportContent, "step_stats.html")
	}
}

// statsPageHandler creates an HTTP handler that displays the current stats.
func statsPageHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reportContent := p.GenerateHTMLMetricsReport()
		servePage(w, r, "Bot-Detector Stats", reportContent, "stats.html")
	}
}
