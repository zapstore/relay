package dbcheck

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestRequireColumn(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // one in-memory database for every query
	if _, err := db.Exec(`CREATE TABLE old (id TEXT); CREATE TABLE current (id TEXT, state TEXT)`); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, table string
		wantErr     bool
	}{
		{"a table without the column", "old", true},
		{"a table with it", "current", false},
		{"a table that does not exist yet", "missing", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RequireColumn(db, tt.table, "state")
			if (err != nil) != tt.wantErr {
				t.Fatalf("RequireColumn(%s) = %v, want error %v", tt.table, err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "add-columns.sh") {
				t.Errorf("error = %q, want it to name the script", err)
			}
		})
	}
}
