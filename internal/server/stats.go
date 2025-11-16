package server

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"bot-detector/internal/logging"
	"bot-detector/internal/types"
)

// Provider defines the interface required by the stats server to access application data.
type Provider interface {
	GetListenAddr() string
	GetShutdownChannel() chan os.Signal
	Log(level logging.LogLevel, tag string, format string, v ...interface{})
	GetConfigForArchive() (mainConfig []byte, modTime time.Time, deps map[string]*types.FileDependency, configPath string, err error)
	GenerateHTMLMetricsReport() string
	GenerateStepsMetricsReport() string
	GetMarshalledConfig() ([]byte, time.Time, error)
}

// Start initializes and starts the HTTP server in a separate goroutine.
func Start(p Provider) {
	listenAddr := p.GetListenAddr()
	if listenAddr == "" {
		p.Log(logging.LevelInfo, "HTTP_SERVER", "HTTP server is disabled.")
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", rootHandler(p))
	mux.HandleFunc("/stats", rootHandler(p)) // Alias for root
	mux.HandleFunc("/stats/steps", stepsHandler(p))
	mux.HandleFunc("/config", configHandler(p))
	mux.HandleFunc("/config/archive", archiveHandler(p))

	server := &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}

	go func() {
		p.Log(logging.LevelInfo, "HTTP_SERVER", "Starting web server on http://%s", listenAddr)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			p.Log(logging.LevelError, "HTTP_SERVER", "Web server failed: %v", err)
		}
	}()

	// Wait for shutdown signal
	<-p.GetShutdownChannel()
	p.Log(logging.LevelInfo, "HTTP_SERVER", "Shutting down web server.")
	if err := server.Close(); err != nil {
		logging.LogOutput(logging.LevelError, "StopServer", "Error stopping server: %v", err)
	}
}

func rootHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, err := fmt.Fprint(w, p.GenerateHTMLMetricsReport())
		if err != nil {
			logging.LogOutput(logging.LevelError, "metricsHandler", "Error writing metrics report: %v", err)
		}
	}
}

func stepsHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, err := fmt.Fprint(w, p.GenerateStepsMetricsReport())
		if err != nil {
			logging.LogOutput(logging.LevelError, "stepsHandler", "Error writing steps report: %v", err)
		}
	}
}
