package relay

import (
	"context"
	"database/sql"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/blossom/store"
	"github.com/zapstore/relay/pkg/policy"
	relaystore "github.com/zapstore/relay/pkg/relay/store"
)

// TestAddColumnsScript runs scripts/add-columns.sh on databases shaped like the
// ones written before pending_events.app_id, ban_notices.kind and id, and
// blobs.state, and checks that the daemons refuse them first and accept them
// after, with the app ids the daemon itself would have stored.
func TestAddColumnsScript(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 is not installed")
	}
	dir := t.TempDir()
	relayPath := filepath.Join(dir, "relay.db")
	blossomPath := filepath.Join(dir, "blossom.db")
	ctx := context.Background()

	// A fresh relay.db, filled the way the daemon fills it.
	events := []*nostr.Event{
		{ID: strings.Repeat("a1", 32), Kind: 32267, Tags: nostr.Tags{{"d", "com.example.app"}}},
		{ID: strings.Repeat("a2", 32), Kind: 30063, Tags: nostr.Tags{{"d", "com.example.app@1"}, {"i", "com.example.app"}}},
		{ID: strings.Repeat("a3", 32), Kind: 3063, Tags: nostr.Tags{{"x", "ff"}, {"i", "com.example.asset"}}},
		{ID: strings.Repeat("a4", 32), Kind: 1, Tags: nostr.Tags{{"i", "not-a-listing"}}},
	}
	db, err := relaystore.New(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if _, err := db.SavePending(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	want := appIDs(t, db.DB)
	if want[events[0].ID] != "com.example.app" || want[events[1].ID] != "com.example.app" ||
		want[events[2].ID] != "com.example.asset" || want[events[3].ID] != "" {
		t.Fatalf("the daemon stored app ids %v", want)
	}
	// The policy store keeps ban_notices in the same file.
	if _, err := policy.Open(db.DB); err != nil {
		t.Fatal(err)
	}
	// Shape it like the old database.
	for _, stmt := range []string{
		`DROP INDEX idx_pending_events_app_id`,
		`ALTER TABLE pending_events DROP COLUMN app_id`,
		`INSERT INTO ban_notices (app_id, reason) VALUES ('com.example.banned', 'spam')`,
		`ALTER TABLE ban_notices DROP COLUMN kind`,
		`ALTER TABLE ban_notices DROP COLUMN id`,
	} {
		if _, err := db.DB.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// A blossom.db without the state column.
	blobs, err := store.New(blossomPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TABLE blobs`,
		`CREATE TABLE blobs (hash TEXT PRIMARY KEY, type TEXT NOT NULL, size INTEGER NOT NULL,
			created_at INTEGER NOT NULL, auth_pubkey TEXT)`,
		`INSERT INTO blobs (hash, size, type, created_at) VALUES ('` + strings.Repeat("b1", 32) + `', 3, 'image/png', 1)`,
	} {
		if _, err := blobs.DB.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := blobs.Close(); err != nil {
		t.Fatal(err)
	}

	// Before the script every daemon refuses its database, naming the script.
	if _, err := relaystore.New(relayPath); err == nil || !strings.Contains(err.Error(), "add-columns.sh") {
		t.Fatalf("relay store on an old database: %v, want a refusal naming the script", err)
	}
	if _, err := store.New(blossomPath); err == nil || !strings.Contains(err.Error(), "add-columns.sh") {
		t.Fatalf("blossom store on an old database: %v, want a refusal naming the script", err)
	}
	old, err := sql.Open("sqlite3", relayPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policy.Open(old); err == nil || !strings.Contains(err.Error(), "add-columns.sh") {
		t.Fatalf("policy on an old database: %v, want a refusal naming the script", err)
	}
	old.Close()

	run := func() string {
		t.Helper()
		out, err := exec.Command("../../scripts/add-columns.sh", "-y", dir).CombinedOutput()
		if err != nil {
			t.Fatalf("script: %v\n%s", err, out)
		}
		return string(out)
	}
	t.Logf("%s", run())

	// After it they open, and the backfill matches what the daemon stored.
	db, err = relaystore.New(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got := appIDs(t, db.DB)
	for id, app := range want {
		if got[id] != app {
			t.Errorf("event %s... has app_id %q, want %q", id[:4], got[id], app)
		}
	}
	if _, err := policy.Open(db.DB); err != nil {
		t.Fatalf("policy after the script: %v", err)
	}
	var notices int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM ban_notices WHERE app_id = 'com.example.banned' AND kind = '' AND id = ''`).Scan(&notices); err != nil || notices != 1 {
		t.Errorf("existing ban notice kept with an empty publisher: %d, %v", notices, err)
	}
	blobs, err = store.New(blossomPath)
	if err != nil {
		t.Fatalf("blossom store after the script: %v", err)
	}
	defer blobs.Close()
	var served int
	if err := blobs.DB.QueryRow(`SELECT COUNT(*) FROM blobs WHERE state = 'served'`).Scan(&served); err != nil || served != 1 {
		t.Errorf("existing blobs are served: %d, %v", served, err)
	}

	// A second run finds nothing to do and writes no further backup.
	db.Close()
	blobs.Close()
	if out := run(); !strings.Contains(out, "nothing to do") {
		t.Errorf("second run: %q, want it to say there is nothing to do", out)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "*.before-add-columns-*"))
	if len(backups) != 2 {
		t.Errorf("backups = %v, want one per database", backups)
	}
}

func appIDs(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT id, app_id FROM pending_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := make(map[string]string)
	for rows.Next() {
		var id, app string
		if err := rows.Scan(&id, &app); err != nil {
			t.Fatal(err)
		}
		ids[id] = app
	}
	return ids
}
