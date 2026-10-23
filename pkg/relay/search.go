package relay

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/relay/store"
	"github.com/zapstore/relay/pkg/search"
	"github.com/zapstore/steroid/artifacts"
)

// SearchListings loads the live selection, one listing per app id as bundles
// choose, with the vector and security file from artifactsPath. A missing
// artifacts.db ranks every listing by text alone.
func SearchListings(db store.T, catalogKey, artifactsPath string) search.Loader {
	return func(ctx context.Context) ([]search.Entry, error) {
		listings, err := selectedListings(ctx, db, catalogKey, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("search: load listings: %w", err)
		}

		arts, err := artifacts.OpenRead(ctx, artifactsPath)
		if errors.Is(err, os.ErrNotExist) {
			arts, err = nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("search: open artifacts: %w", err)
		}
		if arts != nil {
			defer arts.Close()
		}

		entries := make([]search.Entry, 0, len(listings))
		for _, listing := range listings {
			name, _ := events.Find(listing.App.Tags, "name")
			if name == "" {
				continue
			}
			entry := search.Entry{
				App:   search.App{ID: listing.AppID, Name: name},
				Event: listing.App,
			}
			if arts != nil {
				app, err := arts.Load(ctx, listing.AppID)
				if err != nil {
					return nil, fmt.Errorf("search: artifacts for %s: %w", listing.AppID, err)
				}
				if len(app.Vector) == search.VectorDims {
					entry.Vector = make([]int8, search.VectorDims)
					for i, b := range app.Vector {
						entry.Vector[i] = int8(b)
					}
				}
				security := search.ParseSecurity(app.Security)
				entry.FactBits, entry.Keywords = security.FactBits, security.Keywords
			}
			entries = append(entries, entry)
		}
		return entries, nil
	}
}

// UseSearch answers NIP-50 text search from engine.
func (r *T) UseSearch(engine *search.Engine) { r.search = engine }

// textSearch reports whether filters is one NIP-50 text search.
func textSearch(filters nostr.Filters) bool {
	return len(filters) == 1 && filters[0].Search != ""
}

// liveRead reports whether filters is one live read, and whether it is `listing:asset`.
func liveRead(filters nostr.Filters) (assetOnly, ok bool) {
	if !textSearch(filters) {
		return false, false
	}
	return search.LiveRead(filters[0].Search)
}

// searchREQ answers one NIP-50 filter with the selected kind 32267 of each
// hit, in rank order. #f and the other filter fields apply before ranking.
// `filter:<fact>` and `-filter:<fact>` in the search string add and remove facts.
func (r *T) searchREQ(ctx context.Context, filter nostr.Filter) ([]nostr.Event, error) {
	if r.search == nil {
		return nil, fmt.Errorf("%w: text search is not available", store.ErrUnsupportedREQ)
	}
	match := filter
	match.Search = ""
	query, add, remove := search.Extensions(filter.Search)
	result, err := r.search.Search(ctx, search.Request{
		Query:  query,
		Add:    add,
		Remove: remove,
		Match:  func(e nostr.Event) bool { return match.Matches(&e) },
		Limit:  filter.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]nostr.Event, len(result.Hits))
	for i, hit := range result.Hits {
		out[i] = hit.Event
	}
	return out, nil
}
