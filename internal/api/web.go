package api

import (
	"embed"
	"net/http"
)

//go:embed web/index.html
var webFS embed.FS

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}
