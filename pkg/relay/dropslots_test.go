package relay

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zapstore/relay/pkg/relay/store"
)

// TestDropSlotsScript runs scripts/drop-slots.sh on a database shaped like the
// slot design's: a developer's listing hidden next to the catalog's.
func TestDropSlotsScript(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 is not installed")
	}
	const twoListings, catalogOnly, developerOnly = "com.example.app", "com.example.solo", "com.example.dev"
	cert := strings.Repeat("cd", 32)
	catalog := testPubkey(t)
	developer := testPubkey(t)
	path := filepath.Join(t.TempDir(), "relay.db")

	db, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`ALTER TABLE events ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`CREATE INDEX events_hidden_idx ON events(hidden)`); err != nil {
		t.Fatal(err)
	}
	saveListing(t, db, catalog, twoListings, cert, "1", "9")
	saveListing(t, db, developer, twoListings, cert, "2", "2")
	saveListing(t, db, catalog, catalogOnly, cert, "3", "1")
	saveListing(t, db, developer, developerOnly, cert, "4", "1")
	if _, err := db.DB.Exec(`UPDATE events SET hidden = 1 WHERE pubkey = ?`, developer); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("../../scripts/drop-slots.sh", "-y", path, catalog).CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v\n%s", err, out)
	}
	t.Logf("%s", out)

	db, err = store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owners := func(app string) string {
		t.Helper()
		rows, err := db.DB.Query(`SELECT DISTINCT e.pubkey FROM events e JOIN tags t ON t.event_id = e.id
			WHERE t.key IN ('d', 'i') AND t.value = ?`, app)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var pub string
			if err := rows.Scan(&pub); err != nil {
				t.Fatal(err)
			}
			got = append(got, pub)
		}
		return strings.Join(got, ",")
	}
	if got := owners(twoListings); got != developer {
		t.Fatalf("%s listed by %q, want only the developer", twoListings, got)
	}
	if got := owners(catalogOnly); got != catalog {
		t.Fatalf("%s listed by %q, want the catalog key untouched", catalogOnly, got)
	}
	if got := owners(developerOnly); got != developer {
		t.Fatalf("%s listed by %q, want the developer untouched", developerOnly, got)
	}
	var n int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('events') WHERE name = 'hidden'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("events.hidden remains: %d %v", n, err)
	}
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM tags WHERE event_id NOT IN (SELECT id FROM events)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("orphaned tags: %d %v", n, err)
	}
	archive, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "relay.db.removed-slot-events-*.db"))
	if len(archive) == 1 {
		counted, err := exec.Command("sqlite3", archive[0], "SELECT COUNT(*) FROM removed_events").Output()
		if err != nil || strings.TrimSpace(string(counted)) != "3" {
			t.Fatalf("removed_events: %q %v", counted, err)
		}
	}
	for pattern, want := range map[string]int{"relay.db.before-release-chain-*": 1, "relay.db.removed-slot-events-*.db": 1} {
		if found, _ := filepath.Glob(filepath.Join(filepath.Dir(path), pattern)); len(found) != want {
			t.Fatalf("%s: found %v", pattern, found)
		}
	}
}
