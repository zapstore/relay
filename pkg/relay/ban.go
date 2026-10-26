package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/policy"
	"github.com/zapstore/relay/pkg/relay/store"
)

var errBanCatalogKey = errors.New("the catalog key is never banned")

// ApplyBan stores the publisher, removes that publisher's listings, stored
// and in the buffer zone, and selects the apps that still have a hidden slot.
// It returns every app id whose search entry changed. A banned Nostr key also
// loses every other event waiting in the buffer zone.
func ApplyBan(ctx context.Context, db store.T, pol *policy.Store, catalogKey string, pub policy.Publisher, reason string) ([]string, error) {
	pub, err := policy.Normalize(pub.Kind, pub.ID)
	if err != nil {
		return nil, err
	}
	if pub.Kind == policy.KindNostr && pub.ID == catalogKey {
		return nil, errBanCatalogKey
	}
	rows, err := loadListings(ctx, db)
	if err != nil {
		return nil, err
	}
	pending, err := db.ListPending(ctx, bufferedKinds())
	if err != nil {
		return nil, err
	}
	isPending := map[string]struct{}{}
	for _, event := range pending {
		isPending[event.ID] = struct{}{}
		if listingKind(event.Kind) {
			rows = append(rows, policy.Listing{ID: event.ID, Pubkey: event.PubKey, Kind: event.Kind, Tags: event.Tags})
		}
	}
	ids, hashes, apps := policy.Affected(pub, rows)
	if err := pol.RecordBan(ctx, pub, reason, hashes, apps); err != nil {
		return nil, err
	}
	var stored []string
	for _, id := range ids {
		if _, ok := isPending[id]; ok {
			if err := db.DeletePending(ctx, id); err != nil {
				return nil, fmt.Errorf("drop pending event %s: %w", id, err)
			}
			continue
		}
		stored = append(stored, id)
	}
	if pub.Kind == policy.KindNostr {
		for _, event := range pending {
			if event.PubKey != pub.ID {
				continue
			}
			if err := db.DeletePending(ctx, event.ID); err != nil {
				return nil, fmt.Errorf("drop pending event %s: %w", event.ID, err)
			}
		}
	}
	for batch := range slices.Chunk(stored, batchSize) {
		if _, err := db.Delete(ctx, nostr.Filter{IDs: batch}); err != nil {
			return nil, fmt.Errorf("delete banned listings: %w", err)
		}
	}
	hidden, err := selectHidden(ctx, db, catalogKey)
	if err != nil {
		return nil, err
	}
	return append(apps, hidden...), nil
}

// loadListings reads every stored app, release, and asset so a forge or
// certificate ban can find the rows it affects. Worth indexing later.
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
