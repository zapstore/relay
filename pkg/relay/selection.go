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

// reselectBatch bounds the app ids per snapshot query: each id is bound twice.
const reselectBatch = 500

// selectSlots drops kinds 32267, 30063, and 3063 not signed by the signer of
// the app's selected release, so a REQ sees one slot per app as bundles do.
// An app with no selected release is not filtered. A filter naming the
// event's author keeps it: publishers read their own listing whichever slot
// is selected.
func selectSlots(ctx context.Context, db store.T, filters nostr.Filters, result []nostr.Event) ([]nostr.Event, error) {
	appIDs := slotAppIDs(result)
	if len(appIDs) == 0 {
		return result, nil
	}
	args := make([]any, len(appIDs))
	for i, id := range appIDs {
		args[i] = id
	}
	rows, err := db.DB.QueryContext(ctx, `
		SELECT l.app_id, e.pubkey FROM listings l JOIN events e ON e.id = l.release_id
		WHERE l.app_id`+inClause(len(appIDs)), args...)
	if err != nil {
		return nil, fmt.Errorf("slot selection: %w", err)
	}
	defer rows.Close()
	signers := map[string]string{}
	for rows.Next() {
		var appID, pubkey string
		if err := rows.Scan(&appID, &pubkey); err != nil {
			return nil, err
		}
		signers[appID] = pubkey
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	kept := result[:0]
	for _, event := range result {
		appID, _ := slotAppID(event)
		signer, ok := signers[appID]
		if !ok || signer == event.PubKey || namesAuthor(filters, event.PubKey) {
			kept = append(kept, event)
		}
	}
	return kept, nil
}

// reselect stores the selected release of every app id with a kind 32267 and
// no row in listings: the release of the listing catalog selection picks, or
// NULL when none is complete. It reads and writes in one transaction, so a
// write that lands in between fails the commit instead of leaving a stale
// row; the next call fills it.
func reselect(ctx context.Context, db store.T, catalogKey string) error {
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reselect: %w", err)
	}
	defer tx.Rollback()
	ids, err := unselectedApps(ctx, tx)
	if err != nil || len(ids) == 0 {
		return err
	}
	for batch := range slices.Chunk(ids, reselectBatch) {
		snapshot, err := selectionEvents(ctx, tx, batch)
		if err != nil {
			return err
		}
		releases := catalog.SelectedReleases(snapshot, catalogKey, time.Now())
		for _, id := range batch {
			var release sql.NullString
			if event, ok := releases[id]; ok {
				release = sql.NullString{String: event.ID, Valid: true}
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO listings (app_id, release_id) VALUES (?, ?)`, id, release); err != nil {
				return fmt.Errorf("reselect %s: %w", id, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reselect: %w", err)
	}
	return nil
}

// selectedListings returns the listings the listings table holds, in app-id
// order, as a snapshot carries them. appIDs, when set, limits the apps. limit,
// when positive, caps them. It reads only each selected release, its assets,
// and its signer's kind 32267, not the listing history.
func selectedListings(ctx context.Context, db store.T, catalogKey string, appIDs []string, limit int) ([]catalog.Listing, error) {
	if err := reselect(ctx, db, catalogKey); err != nil {
		return nil, err
	}
	query := `SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM listings l JOIN events e ON e.id = l.release_id`
	var args []any
	if len(appIDs) > 0 {
		query += ` WHERE l.app_id` + inClause(len(appIDs))
		for _, id := range appIDs {
			args = append(args, id)
		}
	}
	query += ` ORDER BY l.app_id`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	releases, err := loadEvents(ctx, db.DB, query, args...)
	if err != nil {
		return nil, fmt.Errorf("selected listings: %w", err)
	}
	var out []catalog.Listing
	for batch := range slices.Chunk(releases, reselectBatch) {
		listings, err := completeListings(ctx, db, catalogKey, batch)
		if err != nil {
			return nil, err
		}
		out = append(out, listings...)
	}
	return out, nil
}

// completeListings joins each selected release with its signer's kind 32267
// and its assets, then lets catalog selection choose the assets a snapshot
// carries. Releases are in app-id order and so is the result.
func completeListings(ctx context.Context, db store.T, catalogKey string, releases []nostr.Event) ([]catalog.Listing, error) {
	var appArgs, assetArgs []any
	appArgs = append(appArgs, events.KindApp)
	for _, release := range releases {
		appID, _ := events.Find(release.Tags, "i")
		appArgs = append(appArgs, release.PubKey+":"+appID)
		for _, id := range events.FindAll(release.Tags, "e") {
			assetArgs = append(assetArgs, id)
		}
	}
	apps, err := loadEvents(ctx, db.DB, `
		SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig FROM events e
		JOIN tags t ON t.event_id = e.id AND t.key = 'd'
		WHERE e.kind = ? AND e.pubkey || ':' || t.value`+inClause(len(appArgs)-1), appArgs...)
	if err != nil {
		return nil, fmt.Errorf("selected listings: apps: %w", err)
	}
	var assets []nostr.Event
	if len(assetArgs) > 0 {
		assets, err = loadEvents(ctx, db.DB, `
			SELECT id, pubkey, created_at, kind, tags, content, sig FROM events
			WHERE kind = ? AND id`+inClause(len(assetArgs)), append([]any{events.KindAsset}, assetArgs...)...)
		if err != nil {
			return nil, fmt.Errorf("selected listings: assets: %w", err)
		}
	}
	snapshot := slices.Concat(apps, releases, assets)
	return catalog.ResolveAt(snapshot, catalogKey, time.Now()).Listings, nil
}

func unselectedApps(ctx context.Context, db querier) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT t.value FROM events e JOIN tags t ON t.event_id = e.id AND t.key = 'd'
		WHERE e.kind = ? AND NOT EXISTS (SELECT 1 FROM listings l WHERE l.app_id = t.value)`, events.KindApp)
	if err != nil {
		return nil, fmt.Errorf("reselect: apps: %w", err)
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

func slotAppID(event nostr.Event) (string, bool) {
	switch event.Kind {
	case events.KindApp:
		return events.Find(event.Tags, "d")
	case events.KindRelease, events.KindAsset:
		return events.Find(event.Tags, "i")
	}
	return "", false
}

func slotAppIDs(result []nostr.Event) []string {
	var ids []string
	for _, event := range result {
		if appID, ok := slotAppID(event); ok && appID != "" && !slices.Contains(ids, appID) {
			ids = append(ids, appID)
		}
	}
	return ids
}

func namesAuthor(filters nostr.Filters, pubkey string) bool {
	for _, filter := range filters {
		if slices.Contains(filter.Authors, pubkey) {
			return true
		}
	}
	return false
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// selectionEvents loads the listings of appIDs: kinds 32267, 30063, and 3063.
func selectionEvents(ctx context.Context, db querier, appIDs []string) ([]nostr.Event, error) {
	args := []any{events.KindApp}
	for _, id := range appIDs {
		args = append(args, id)
	}
	args = append(args, events.KindRelease, events.KindAsset)
	for _, id := range appIDs {
		args = append(args, id)
	}
	listings, err := loadEvents(ctx, db, `
		SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig FROM events e
		WHERE e.id IN (
			SELECT t.event_id FROM tags t JOIN events x ON x.id = t.event_id
			WHERE x.kind = ? AND t.key = 'd' AND t.value`+inClause(len(appIDs))+`
			UNION
			SELECT t.event_id FROM tags t JOIN events x ON x.id = t.event_id
			WHERE x.kind IN (?, ?) AND t.key = 'i' AND t.value`+inClause(len(appIDs))+`
		)`, args...)
	if err != nil {
		return nil, fmt.Errorf("slot selection: listings: %w", err)
	}
	return listings, nil
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
