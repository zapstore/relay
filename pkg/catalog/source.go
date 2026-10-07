package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func LoadEvents(ctx context.Context, dbPath, stackPubkey, catalogPubkey string) ([]nostr.Event, error) {
	db, err := openRelayDB(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `
		SELECT id, pubkey, created_at, kind, tags, content, sig
		FROM events
		WHERE kind IN (?, ?, ?, ?, ?)
		   OR (kind = ? AND pubkey = ?)
		   OR (kind = ? AND pubkey = ?)
	`, events.KindApp, events.KindRelease, events.KindAsset, events.KindIdentityProof, events.KindProfile,
		events.KindStack, stackPubkey,
		events.KindAppRelays, catalogPubkey)
	if err != nil {
		return nil, fmt.Errorf("query catalog events: %w", err)
	}
	defer rows.Close()
	var out []nostr.Event
	for rows.Next() {
		var event nostr.Event
		if err := rows.Scan(&event.ID, &event.PubKey, &event.CreatedAt, &event.Kind, &event.Tags, &event.Content, &event.Sig); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

// openRelayDB reads a live relay database. It does not apply schema or PRAGMA optimize,
// both of which take a write lock and fail while the relay is writing.
func openRelayDB(ctx context.Context, path string) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_query_only=1&_busy_timeout=10000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open relay database %s: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open relay database %s: %w", path, err)
	}
	return db, nil
}
