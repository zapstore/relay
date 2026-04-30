// Package store provides a SQLite-backed storage layer for analytics data.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schema string

// T is the store type that manages the analytics SQLite database.
type T struct {
	db *sql.DB
}

// New opens (or creates) the SQLite database at the given path and applies the schema.
func New(path string) (*T, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to sqlite3 at %s: %w", path, err)
	}

	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("failed to apply base schema: %w", err)
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("failed to migrate: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL;"); err != nil {
		return nil, fmt.Errorf("failed to set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000;"); err != nil {
		return nil, fmt.Errorf("failed to set busy timeout: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		return nil, fmt.Errorf("failed to activate foreign keys: %w", err)
	}
	if _, err = db.Exec("PRAGMA optimize=0x10002;"); err != nil {
		return nil, fmt.Errorf("failed to PRAGMA optimize: %w", err)
	}

	return &T{db: db}, nil
}

// Close closes the underlying database connection.
func (s *T) Close() error {
	return s.db.Close()
}

func migrate(db *sql.DB) error {
	// Additive migration: add type column to existing databases.
	// New databases get the column via schema.sql; existing rows default to 'unknown'.
	_, err := db.Exec(`ALTER TABLE downloads ADD COLUMN type TEXT NOT NULL DEFAULT 'unknown'`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return fmt.Errorf("add type column: %w", err)
	}
	return nil
}
