package relay

import (
	"context"
	"fmt"
	"slices"

	"github.com/nbd-wtf/go-nostr"
)

// liveREQ answers a live read from the listings table: for each selected app, in app-id order, the
// kind 32267, then either its kind 30063 and the snapshot's kind 3063 assets
// in the release's `e` order, or, for `listing:asset`, the one asset with the
// greatest version_code (the lowest event id on a tie). `#d` names the apps;
// without it every app a snapshot carries is read. `limit` caps the apps.
// budget, when positive, caps the events: the read stops before an app whose
// events would not all fit the client's free response slots, since rely drops
// what does not fit and then disconnects the client.
func (r *T) liveREQ(ctx context.Context, filter nostr.Filter, assetOnly bool, budget int) ([]nostr.Event, error) {
	limit := filter.Limit
	if budget > 0 {
		// Every app sends at least two events, so no more apps than this fit.
		if most := budget / 2; limit <= 0 || most < limit {
			limit = max(most, 1)
		}
	}
	listings, err := selectedListings(ctx, r.store, r.config.Info.Pubkey, filter.Tags["d"], limit)
	if err != nil {
		return nil, fmt.Errorf("live read: %w", err)
	}

	var out []nostr.Event
	for _, listing := range listings {
		events := []nostr.Event{listing.App, listing.Assets[0]}
		if !assetOnly {
			events = append([]nostr.Event{listing.App, listing.Release}, releaseOrder(listing.Release, listing.Assets)...)
		}
		if budget > 0 && len(out)+len(events) > budget {
			break
		}
		out = append(out, events...)
	}
	return out, nil
}

// releaseOrder sorts assets by their position in the release's `e` tags.
func releaseOrder(release nostr.Event, assets []nostr.Event) []nostr.Event {
	position := map[string]int{}
	for _, tag := range release.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			if _, seen := position[tag[1]]; !seen {
				position[tag[1]] = len(position)
			}
		}
	}
	sorted := slices.Clone(assets)
	slices.SortStableFunc(sorted, func(a, b nostr.Event) int {
		return position[a.ID] - position[b.ID]
	})
	return sorted
}
