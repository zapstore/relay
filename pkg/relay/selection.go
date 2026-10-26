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

// An app has at most two slots: the catalog key's and one verified
// developer's. With one slot nothing is hidden. With two, selectApp runs
// catalog selection for that app and sets events.hidden on the kinds 32267,
// 30063, and 3063 of the slot that lost, so a REQ sees one slot per app.

// batchSize bounds the ids bound in one statement.
const batchSize = 500

// appEvents is the subquery of the ids of an app's kinds 32267, 30063, and
// 3063: d names the app on 32267, i on the others. It binds the app id.
const appEvents = `SELECT t.event_id FROM tags t JOIN events x ON x.id = t.event_id
	WHERE t.value = ? AND ((x.kind = 32267 AND t.key = 'd') OR (x.kind IN (30063, 3063) AND t.key = 'i'))`

// selectApp sets hidden on appID's events. With one signer, nothing is
// hidden. With two, the signer of the release catalog selection picks keeps
// the app and the other is hidden; when neither listing is complete,
// nothing is hidden.
func selectApp(ctx context.Context, db store.T, catalogKey, appID string) error {
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("select %s: %w", appID, err)
	}
	defer tx.Rollback()
	var signers int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT e.pubkey) FROM events e WHERE e.id IN (`+appEvents+`)`, appID).Scan(&signers)
	if err != nil {
		return fmt.Errorf("select %s: signers: %w", appID, err)
	}
	winner := ""
	if signers > 1 {
		snapshot, err := selectionEvents(ctx, tx, appID)
		if err != nil {
			return err
		}
		if release, ok := catalog.SelectedReleases(snapshot, catalogKey, time.Now())[appID]; ok {
			winner = release.PubKey
		}
	}
	hide := `(? != '' AND pubkey != ?)`
	_, err = tx.ExecContext(ctx, `UPDATE events SET hidden = `+hide+`
		WHERE hidden != `+hide+` AND id IN (`+appEvents+`)`,
		winner, winner, winner, winner, appID)
	if err != nil {
		return fmt.Errorf("select %s: %w", appID, err)
	}
	return tx.Commit()
}

// selectHidden runs selectApp for every app with a hidden event. A deletion
// calls it: removing the selected slot's events may leave the other slot
// the only one, or the better one.
func selectHidden(ctx context.Context, db store.T, catalogKey string) ([]string, error) {
	ids, err := appIDs(ctx, db.DB, `
		SELECT DISTINCT t.value FROM events e JOIN tags t ON t.event_id = e.id
		WHERE e.hidden = 1 AND ((e.kind = ? AND t.key = 'd') OR (e.kind IN (?, ?) AND t.key = 'i'))`,
		events.KindApp, events.KindRelease, events.KindAsset)
	if err != nil {
		return nil, fmt.Errorf("select hidden: %w", err)
	}
	for _, id := range ids {
		if err := selectApp(ctx, db, catalogKey, id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// SelectAll runs selectApp for every app with two signers or a hidden
// event. It is for a database written without the relay, like launch's.
func SelectAll(ctx context.Context, db store.T, catalogKey string) error {
	ids, err := appIDs(ctx, db.DB, `
		SELECT t.value FROM events e JOIN tags t ON t.event_id = e.id AND t.key = 'd'
		WHERE e.kind = ? GROUP BY t.value HAVING COUNT(DISTINCT e.pubkey) > 1`, events.KindApp)
	if err != nil {
		return fmt.Errorf("select all: %w", err)
	}
	for _, id := range ids {
		if err := selectApp(ctx, db, catalogKey, id); err != nil {
			return err
		}
	}
	if _, err := selectHidden(ctx, db, catalogKey); err != nil {
		return err
	}
	return nil
}

// visibleApps returns the kind 32267 events that are not hidden, one per
// app id, in app-id order. appIDs, when set, limits the apps. limit, when
// positive, caps them.
func visibleApps(ctx context.Context, db store.T, appIDs []string, limit int) ([]nostr.Event, error) {
	query := `SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM events e JOIN tags t ON t.event_id = e.id AND t.key = 'd'
		WHERE e.kind = ? AND e.hidden = 0`
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
	apps, err := loadEvents(ctx, db.DB, query, args...)
	if err != nil {
		return nil, fmt.Errorf("visible apps: %w", err)
	}
	return apps, nil
}

// selectedListings returns the listings of the visible apps, in app-id
// order, as a snapshot carries them: each app's release with the greatest
// version_code and its assets. appIDs and limit are as in visibleApps. An
// app with no complete listing is left out.
func selectedListings(ctx context.Context, db store.T, catalogKey string, appIDs []string, limit int) ([]catalog.Listing, error) {
	apps, err := visibleApps(ctx, db, appIDs, limit)
	if err != nil {
		return nil, err
	}
	var out []catalog.Listing
	for batch := range slices.Chunk(apps, batchSize) {
		args := []any{events.KindRelease, events.KindAsset}
		for _, app := range batch {
			appID, _ := events.Find(app.Tags, "d")
			args = append(args, appID)
		}
		releases, err := loadEvents(ctx, db.DB, `
			SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig FROM events e
			WHERE e.kind IN (?, ?) AND e.hidden = 0
			AND e.id IN (SELECT event_id FROM tags WHERE key = 'i' AND value`+inClause(len(batch))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("selected listings: %w", err)
		}
		snapshot := slices.Concat(batch, releases)
		out = append(out, catalog.ResolveAt(snapshot, catalogKey, time.Now()).Listings...)
	}
	return out, nil
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

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// selectionEvents loads the kinds 32267, 30063, and 3063 of appID.
func selectionEvents(ctx context.Context, db querier, appID string) ([]nostr.Event, error) {
	listing, err := loadEvents(ctx, db, `
		SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig FROM events e
		WHERE e.id IN (`+appEvents+`)`, appID)
	if err != nil {
		return nil, fmt.Errorf("select %s: listing: %w", appID, err)
	}
	return listing, nil
}

func appIDs(ctx context.Context, db querier, query string, args ...any) ([]string, error) {
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
