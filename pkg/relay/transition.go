package relay

// TODO(transition): remove this file once all publishers include the h tag and legacy
// events without it are no longer relevant. Also revert the withLegacyHTagFallback call
// and the conditional deduplicateEvents call in Query.

import (
	"slices"

	"github.com/nbd-wtf/go-nostr"
)

// zapstoreCommunityPubkey is the value used in the `h` tag to identify events
// belonging to the Zapstore community.
const zapstoreCommunityPubkey = "acfeaea6e51420e8068fac446ca9d17d7a9ef6a5d20d93894e50fee3d4902a84"

// withLegacyHTagFallback expands filters that contain h=#zapstoreCommunityPubkey with
// an additional filter that omits the h-tag constraint entirely, so that events
// published before the h tag was adopted are also returned.
//
// Returns the (possibly expanded) filter set and whether any expansion occurred.
// When expanded is true the caller must deduplicate results, since events carrying
// h=zapstoreCommunityPubkey will match both the original and the expanded filter.
func withLegacyHTagFallback(filters nostr.Filters) (out nostr.Filters, expanded bool) {
	out = make(nostr.Filters, 0, len(filters))
	for _, f := range filters {
		out = append(out, f)
		hVals, hasH := f.Tags["h"]
		if hasH && slices.Contains(hVals, zapstoreCommunityPubkey) {
			out = append(out, filterWithoutTag(f, "h"))
			expanded = true
		}
	}
	return out, expanded
}

// filterWithoutTag returns a copy of f with the named tag key removed from Tags.
func filterWithoutTag(f nostr.Filter, key string) nostr.Filter {
	out := f
	newTags := make(nostr.TagMap, len(f.Tags))
	for k, v := range f.Tags {
		if k != key {
			newTags[k] = v
		}
	}
	if len(newTags) == 0 {
		out.Tags = nil
	} else {
		out.Tags = newTags
	}
	return out
}

// deduplicateEvents removes duplicate events by ID, preserving order.
func deduplicateEvents(evs []nostr.Event) []nostr.Event {
	seen := make(map[string]struct{}, len(evs))
	out := make([]nostr.Event, 0, len(evs))
	for _, e := range evs {
		if _, ok := seen[e.ID]; ok {
			continue
		}
		seen[e.ID] = struct{}{}
		out = append(out, e)
	}
	return out
}
