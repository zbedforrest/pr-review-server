package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"pr-review-server/auth"
	"pr-review-server/db"
)

const exportPath = "/api/admin/export"

const (
	defaultExportLimit = 1000
	maxExportLimit     = 5000
)

type exportReader interface {
	ExportRows(table string, offset, limit int) ([]map[string]any, error)
}

// handleExport returns one page of an allowlisted ledger table for offline
// analysis, so admins never need the database credentials.
// GET ?table=<name>&offset=<n>&limit=<n>; without table it lists the tables.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := auth.GetCurrentUser(r)
	if user == nil {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}
	if !s.isAdmin(user) {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	reader, ok := s.db.(exportReader)
	if !ok {
		http.Error(w, "export unavailable", http.StatusNotImplemented)
		return
	}
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Content-Type", "application/json")
	q := r.URL.Query()
	table := q.Get("table")
	if table == "" {
		_ = json.NewEncoder(w).Encode(map[string]any{"tables": db.ExportTableNames()}) // nolint:errcheck
		return
	}
	offset, ok := nonNegativeParam(w, q.Get("offset"), "offset", 0)
	if !ok {
		return
	}
	limit, ok := nonNegativeParam(w, q.Get("limit"), "limit", defaultExportLimit)
	if !ok {
		return
	}
	limit = min(max(limit, 1), maxExportLimit)
	rows, err := reader.ExportRows(table, offset, limit)
	if err != nil {
		log.Printf("[API] export %s: %v", table, err)
		http.Error(w, "export failed", http.StatusBadRequest)
		return
	}
	next := -1
	if len(rows) == limit {
		next = offset + limit
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"table": table, "offset": offset, "rows": rows, "next_offset": next}) // nolint:errcheck
}

func nonNegativeParam(w http.ResponseWriter, raw, name string, def int) (int, bool) {
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		http.Error(w, name+" must be a non-negative integer", http.StatusBadRequest)
		return 0, false
	}
	return n, true
}
