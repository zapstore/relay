package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestDropForumPosts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.db")
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE events (id TEXT PRIMARY KEY, kind INTEGER)`,
		`CREATE TABLE tags (event_id TEXT, key TEXT, value TEXT)`,
		`INSERT INTO events (id, kind) VALUES ('post', 11), ('reply', 1111), ('appcomment', 1111), ('app', 32267)`,
		`INSERT INTO tags (event_id, key, value) VALUES ('reply', 'e', 'post'), ('appcomment', 'e', 'post'), ('appcomment', 'A', '32267:aa:com.example')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	posts, replies, err := dropForumPosts(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if posts != 1 || replies != 1 {
		t.Fatalf("deleted %d posts and %d replies, want 1 and 1", posts, replies)
	}
	left := idsOf(t, path)
	if len(left) != 2 || left["appcomment"] != 1111 || left["app"] != 32267 {
		t.Fatalf("remaining events: %v", left)
	}
}

func idsOf(t *testing.T, path string) map[string]int {
	t.Helper()
	db, err := openDB(t.Context(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, kind FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var kind int
		if err := rows.Scan(&id, &kind); err != nil {
			t.Fatal(err)
		}
		out[id] = kind
	}
	return out
}
