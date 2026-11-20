package db

import (
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm"
)

// exportTables lists the tables an admin may export, each with the column it
// pages on and the bulky columns left out. Tables holding sessions, users,
// settings or other credentials are never listed.
var exportTables = map[string]struct {
	key     string
	exclude []string
}{
	"prs":                    {key: "id", exclude: []string{"review_run_json", "notes"}},
	"review_runs":            {key: "run_id", exclude: []string{"requested_config_json", "config_sources_json"}},
	"review_stage_attempts":  {key: "id"},
	"published_findings":     {key: "id"},
	"published_reply_models": {key: "id"},
	"settled_points":         {key: "id"},
	"feedback_items":         {key: "id"},
	"comment_grades":         {key: "id"},
	"finding_outcomes":       {key: "id"},
}

// ExportTableNames returns the exportable tables in name order.
func ExportTableNames() []string {
	names := make([]string, 0, len(exportTables))
	for n := range exportTables {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ExportRows reads one page of an allowlisted table inside a read-only
// transaction, ordered by the table's key.
func (g *GormDB) ExportRows(table string, offset, limit int) ([]map[string]any, error) {
	spec, ok := exportTables[table]
	if !ok {
		return nil, fmt.Errorf("table %q is not exportable", table)
	}
	var rows []map[string]any
	err := g.db.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SET TRANSACTION READ ONLY").Error; err != nil {
				return err
			}
		}
		if err := tx.Table(table).Order(spec.key).Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		for _, c := range spec.exclude {
			delete(r, c)
		}
		for k, v := range r {
			if b, ok := v.([]byte); ok {
				r[k] = strings.ToValidUTF8(string(b), "")
			}
		}
	}
	return rows, nil
}
