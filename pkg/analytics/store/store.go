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
	db   *sql.DB
	path string
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
	return &T{db: db, path: path}, nil
}

// Close closes the underlying database connection.
func (s *T) Close() error {
	return s.db.Close()
}

// Path returns the path to the SQLite database file.
func (s *T) Path() string {
	return s.path
}

// inClause returns an SQL IN clause for the given number of placeholders, with the first placeholder repeated n times.
func inClause(n int) string {
	if n <= 0 {
		panic("analytics.store.inClause: n must be positive")
	}
	if n == 1 {
		return "= ?"
	}
	return "IN (" + strings.Repeat("?,", n-1) + "?)"
}
