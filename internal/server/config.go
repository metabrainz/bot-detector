package server

import (
	"bytes"
	"net/http"
)

// configHandler creates an HTTP handler that displays the current config.
func configHandler(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, modtime, err := p.GetMarshalledConfig()
		if err != nil || len(data) == 0 {
			http.Error(w, "failed to read config file", http.StatusInternalServerError)
			return
		}
		http.ServeContent(w, r, "config.yaml", modtime, bytes.NewReader(data))
	}
}
