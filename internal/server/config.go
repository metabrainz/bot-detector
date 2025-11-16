package server

import (
	"bot-detector/internal/logging"
	"net/http"
)

func configHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		config, modTime, err := p.GetMarshalledConfig()
		if err != nil {
			http.Error(w, "failed to read config file", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Last-Modified", modTime.UTC().Format(http.TimeFormat))
		_, err = w.Write(config)
		if err != nil {
			logging.LogOutput(logging.LevelError, "configHandler", "Error writing config: %v", err)
		}
	}
}
