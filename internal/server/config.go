package server

import (
	"net/http"
)

// configHandler creates an HTTP handler that displays the current config.
func configHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		configYAML, err := p.GetMarshalledConfig()
		if err != nil {
			http.Error(w, "failed to read config file", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/x-yaml")
		_, _ = w.Write(configYAML)
	}
}
