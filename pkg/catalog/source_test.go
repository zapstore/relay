package catalog

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/zapstore/relay/pkg/events"
)

func TestLoadEventsReadsWhileAWriterHoldsTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	writer, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`
		PRAGMA journal_mode = WAL;
		CREATE TABLE events (
			id TEXT PRIMARY KEY,
			pubkey TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			kind INTEGER NOT NULL,
			tags TEXT NOT NULL,
			content TEXT NOT NULL,
			sig TEXT NOT NULL
		);
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(
		`INSERT INTO events (id, pubkey, created_at, kind, tags, content, sig) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"id", "aa", 1, events.KindApp, "[]", "", "sig",
	); err != nil {
		t.Fatal(err)
	}
	tx, err := writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE events SET content = 'writing' WHERE id = 'id'`); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got, err := LoadEvents(ctx, path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "id" || got[0].Kind != events.KindApp {
		t.Fatalf("%+v", got)
	}
}
