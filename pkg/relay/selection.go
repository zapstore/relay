package relay

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/catalog"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/relay/store"
)

// Every stored listing is served. Selection happens on read, for bundles,
// live reads and search: the listing with the greatest version_code, and on
// a tie the later release.

// batchSize bounds the ids bound in one statement.
const batchSize = 500

// listedAppIDs is the app ids with a stored kind 32267, in order. appIDs,
// when set, limits them. limit, when positive, caps them.
func listedAppIDs(ctx context.Context, db store.T, appIDs []string, limit int) ([]string, error) {
	query := `SELECT DISTINCT t.value FROM events e JOIN tags t ON t.event_id = e.id AND t.key = 'd'
		WHERE e.kind = ?`
	args := []any{events.KindApp}
	if len(appIDs) > 0 {
		query += ` AND t.value` + inClause(len(appIDs))
		for _, id := range appIDs {
			args = append(args, id)
		}
	}
	query += ` ORDER BY t.value`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	ids, err := appIDsOf(ctx, db.DB, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listed apps: %w", err)
	}
	return ids, nil
}

// selectedListings returns the selected listing of each app, in app-id
// order, as a snapshot carries it: the release with the greatest
// version_code and its assets. appIDs and limit are as in listedAppIDs. An
// app with no complete listing is left out.
func selectedListings(ctx context.Context, db store.T, catalogKey string, appIDs []string, limit int) ([]catalog.Listing, error) {
	ids, err := listedAppIDs(ctx, db, appIDs, limit)
	if err != nil {
		return nil, err
	}
	var out []catalog.Listing
	for batch := range slices.Chunk(ids, batchSize) {
		args := []any{events.KindApp}
		for _, id := range batch {
			args = append(args, id)
		}
		args = append(args, events.KindRelease, events.KindAsset)
		for _, id := range batch {
			args = append(args, id)
		}
		snapshot, err := loadEvents(ctx, db.DB, `
			SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig FROM events e
			WHERE (e.kind = ? AND e.id IN (SELECT event_id FROM tags WHERE key = 'd' AND value`+inClause(len(batch))+`))
			OR (e.kind IN (?, ?) AND e.id IN (SELECT event_id FROM tags WHERE key = 'i' AND value`+inClause(len(batch))+`))`, args...)
		if err != nil {
			return nil, fmt.Errorf("selected listings: %w", err)
		}
		out = append(out, catalog.ResolveAt(snapshot, catalogKey, time.Now()).Listings...)
	}
	return out, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func appIDsOf(ctx context.Context, db querier, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadEvents(ctx context.Context, db querier, query string, args ...any) ([]nostr.Event, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
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

func inClause(n int) string {
	if n == 1 {
		return " = ?"
	}
	return " IN (?" + strings.Repeat(",?", n-1) + ")"
}
