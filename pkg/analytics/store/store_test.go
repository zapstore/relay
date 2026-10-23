package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

var ctx = context.Background()

func TestNewDropsRetiredTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analytics.db")
	old, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`
		CREATE TABLE app_impressions (app_id TEXT);
		CREATE TABLE blossom_metrics (day DATE PRIMARY KEY);
		CREATE TABLE app_downloads (hash TEXT, day DATE, source TEXT);
		CREATE INDEX app_downloads_source ON app_downloads (source);
	`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	var left int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN
		('app_impressions', 'blossom_metrics', 'app_downloads', 'app_downloads_source')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d retired tables or indexes remain", left)
	}
}
