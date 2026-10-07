package relay

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/policy"
	"github.com/zapstore/relay/pkg/relay/store"
)

// ApplyBan stores the publisher and removes that publisher's listings.
func ApplyBan(ctx context.Context, db store.T, pol *policy.Store, pub policy.Publisher, reason string) error {
	rows, err := loadListings(ctx, db)
	if err != nil {
		return err
	}
	ids, hashes, apps := policy.Affected(pub, rows)
	if err := pol.RecordBan(ctx, pub, reason, hashes, apps); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := db.Delete(ctx, nostr.Filter{IDs: []string{id}}); err != nil {
			return fmt.Errorf("delete event %s: %w", id, err)
		}
	}
	return nil
}

// loadListings reads every app, release, and asset so a forge or certificate
// ban can find the rows it affects. This is the whole table and it runs inside
// the event-hook timeout. Worth indexing later.
func loadListings(ctx context.Context, db store.T) ([]policy.Listing, error) {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, pubkey, kind, tags FROM events
		WHERE kind IN (?, ?, ?)
	`, events.KindApp, events.KindRelease, events.KindAsset)
	if err != nil {
		return nil, fmt.Errorf("list listings: %w", err)
	}
	defer rows.Close()
	var out []policy.Listing
	for rows.Next() {
		var row policy.Listing
		var raw string
		if err := rows.Scan(&row.ID, &row.Pubkey, &row.Kind, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &row.Tags); err != nil {
			return nil, fmt.Errorf("tags for %s: %w", row.ID, err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
