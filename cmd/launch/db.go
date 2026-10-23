package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
	"github.com/nbd-wtf/go-nostr"
)

// openDB opens a database of a stopped service. Read-only unless write.
func openDB(ctx context.Context, path string, write bool) (*sql.DB, error) {
	mode := "ro"
	if write {
		mode = "rw"
	}
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+"?mode="+mode+"&_busy_timeout=10000")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

// eventsBy returns every stored event signed by pubkey.
func eventsBy(ctx context.Context, db *sql.DB, pubkey string) ([]nostr.Event, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, pubkey, created_at, kind, json(tags), content, sig
		FROM events WHERE pubkey = ?`, pubkey)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()
	var out []nostr.Event
	for rows.Next() {
		var e nostr.Event
		var tags string
		if err := rows.Scan(&e.ID, &e.PubKey, &e.CreatedAt, &e.Kind, &tags, &e.Content, &e.Sig); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(tags), &e.Tags); err != nil {
			return nil, fmt.Errorf("event %s tags: %w", e.ID, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// pendingBy returns the buffer-zone events signed by pubkey.
func pendingBy(ctx context.Context, db *sql.DB, pubkey string) ([]nostr.Event, error) {
	rows, err := db.QueryContext(ctx, `SELECT raw FROM pending_events`)
	if err != nil {
		return nil, fmt.Errorf("query pending events: %w", err)
	}
	defer rows.Close()
	var out []nostr.Event
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var e nostr.Event
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			continue
		}
		if e.PubKey == pubkey {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

// dropFTS drops the v1 apps_fts index and its triggers. Dropping the table
// needs a build with -tags fts5; the relay itself no longer does.
func dropFTS(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		`DROP TRIGGER IF EXISTS app_fts_ai`,
		`DROP TRIGGER IF EXISTS app_fts_ad`,
		`DROP TABLE IF EXISTS apps_fts`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("drop apps_fts: %w", err)
		}
	}
	return nil
}

func dropRelayFTS(ctx context.Context, relayData string) error {
	db, err := openDB(ctx, filepath.Join(relayData, "relay.db"), true)
	if err != nil {
		return err
	}
	defer db.Close()
	return dropFTS(ctx, db)
}

// dropForumPosts deletes kind 11 and the kind 1111 replies that name those
// posts. A comment with an app or stack address stays. Returns how many
// posts and replies were deleted. The launch backup is taken before this.
func dropForumPosts(ctx context.Context, relayData string) (posts, replies int, err error) {
	db, err := openDB(ctx, filepath.Join(relayData, "relay.db"), true)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	replies, err = deleteWhere(ctx, db, `
		c.kind = 1111
		AND EXISTS (
			SELECT 1 FROM tags t
			JOIN events p ON p.id = t.value AND p.kind = 11
			WHERE t.event_id = c.id AND t.key IN ('e', 'E')
		)
		AND NOT EXISTS (
			SELECT 1 FROM tags a
			WHERE a.event_id = c.id AND a.key IN ('A', 'a')
			AND (a.value LIKE '32267:%' OR a.value LIKE '30267:%')
		)`)
	if err != nil {
		return 0, 0, err
	}
	posts, err = deleteWhere(ctx, db, `c.kind = 11`)
	return posts, replies, err
}

// deleteWhere deletes events matching where, which must use the alias c, and
// their tag rows. It returns how many events were deleted.
func deleteWhere(ctx context.Context, db *sql.DB, where string) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Snapshot the ids first. The reply predicate joins tags, so deleting
	// those rows before the events would make the same predicate match nothing.
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS doomed`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE doomed (id TEXT PRIMARY KEY)`); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO doomed SELECT c.id FROM events c WHERE `+where)
	if err != nil {
		return 0, fmt.Errorf("select forum events: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE event_id IN (SELECT id FROM doomed)`); err != nil {
		return 0, fmt.Errorf("delete forum tags: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id IN (SELECT id FROM doomed)`); err != nil {
		return 0, fmt.Errorf("delete forum events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE doomed`); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// deleteEvents deletes stored and pending events by id, with their tag rows.
// SQLite does not enforce the tags foreign key unless the pragma is on.
func deleteEvents(ctx context.Context, db *sql.DB, ids []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM tags WHERE event_id = ?`,
		`DELETE FROM events WHERE id = ?`,
		`DELETE FROM pending_events WHERE id = ?`,
	} {
		stmt, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := stmt.ExecContext(ctx, id); err != nil {
				stmt.Close()
				return fmt.Errorf("delete %s: %w", id, err)
			}
		}
		if err := stmt.Close(); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// appIDsSignedByOthers is every app id with a kind 32267 from a key other than pubkey.
func appIDsSignedByOthers(ctx context.Context, db *sql.DB, pubkey string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT t.value FROM events e
		JOIN tags t ON t.event_id = e.id AND t.key = 'd'
		WHERE e.kind = 32267 AND e.pubkey != ?`, pubkey)
	if err != nil {
		return nil, fmt.Errorf("query developer listings: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

type disabledApp struct {
	ID     string
	Reason string
}

func disabledApps(ctx context.Context, db *sql.DB) ([]disabledApp, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT app_id, COALESCE(disable_reason, '') FROM apps
		WHERE enabled = 0 ORDER BY app_id`)
	if err != nil {
		return nil, fmt.Errorf("query disabled apps: %w", err)
	}
	defer rows.Close()
	var out []disabledApp
	for rows.Next() {
		var a disabledApp
		if err := rows.Scan(&a.ID, &a.Reason); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func enableApps(ctx context.Context, db *sql.DB, ids []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE apps SET enabled = 1, disable_reason = NULL WHERE app_id = ?`, id); err != nil {
			return fmt.Errorf("enable %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// lineageOf returns the indexer's ancestors for that app and certificate, oldest first.
func lineageOf(ctx context.Context, db *sql.DB, appID, cert string) ([]string, error) {
	var raw string
	err := db.QueryRowContext(ctx, `SELECT lineage FROM app_certificates WHERE app_id = ? AND hash = ?`, appID, cert).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lineage of %s: %w", appID, err)
	}
	var lineage []string
	if err := json.Unmarshal([]byte(raw), &lineage); err != nil {
		return nil, fmt.Errorf("lineage of %s: %w", appID, err)
	}
	return lineage, nil
}
