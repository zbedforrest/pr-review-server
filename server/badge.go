package server

import (
	"embed"
	"net/http"
	"strings"
)

// Severity badges referenced from PRism's GitHub comments. Served without
// auth because GitHub's image proxy fetches them anonymously; the files are
// static and identical for every viewer.
//
//go:embed badges/*.svg
var badgeFS embed.FS

const badgePath = "/badge/"

func (s *Server) handleBadge(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, badgePath)
	switch name {
	case "critical.svg", "medium.svg", "low.svg":
	default:
		http.NotFound(w, r)
		return
	}
	data, err := badgeFS.ReadFile("badges/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}
