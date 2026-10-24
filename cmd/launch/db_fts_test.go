//go:build fts5

package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestDropFTS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.db")
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE VIRTUAL TABLE apps_fts USING fts5(body)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE events (id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER app_fts_ai AFTER INSERT ON events BEGIN SELECT 1; END`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dropRelayFTS(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	db, err = openDB(t.Context(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('apps_fts', 'app_fts_ai')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("apps_fts still present (%d rows)", n)
	}
}

func TestRequireFTS5BuiltIn(t *testing.T) {
	if err := requireFTS5(); err != nil {
		t.Fatal(err)
	}
}
