package catalog

import (
	"slices"
	"strings"
	"time"

	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestChooseListingByVersionThenTime(t *testing.T) {
	const catalogKey = "cc"
	indexer := slotListing(catalogKey, "10", "2", 100)
	developer := slotListing("dd", "11", "2", 200)

	chosen, ok := chooseListing([]Listing{indexer, developer}, catalogKey)
	if !ok || chosen.App.PubKey != "dd" {
		t.Fatalf("later tie chose %q ok=%v", chosen.App.PubKey, ok)
	}
	chosen, _ = chooseListing([]Listing{developer, indexer}, catalogKey)
	if chosen.App.PubKey != "dd" {
		t.Fatalf("later tie in reverse order chose %q", chosen.App.PubKey)
	}

	laterIndexer := slotListing(catalogKey, "12", "2", 300)
	chosen, _ = chooseListing([]Listing{laterIndexer, developer}, catalogKey)
	if chosen.App.PubKey != catalogKey {
		t.Fatalf("later indexer tie chose %q", chosen.App.PubKey)
	}

	sameTime := slotListing("ee", "09", "2", 200)
	chosen, _ = chooseListing([]Listing{developer, sameTime}, catalogKey)
	if chosen.App.PubKey != "ee" {
		t.Fatalf("same time chose %q, want the lower release id", chosen.App.PubKey)
	}

	newerIndexer := slotListing(catalogKey, "13", "3", 50)
	chosen, _ = chooseListing([]Listing{newerIndexer, developer}, catalogKey)
	if chosen.App.PubKey != catalogKey {
		t.Fatalf("higher indexer chose %q", chosen.App.PubKey)
	}

	newerDeveloper := slotListing("dd", "14", "9", 50)
	chosen, _ = chooseListing([]Listing{indexer, newerDeveloper}, catalogKey)
	if chosen.App.PubKey != "dd" {
		t.Fatalf("higher developer chose %q", chosen.App.PubKey)
	}

	if chosen, ok := chooseListing([]Listing{developer}, catalogKey); !ok || chosen.App.PubKey != "dd" {
		t.Fatalf("developer-only app chose %q ok=%v", chosen.App.PubKey, ok)
	}
	if _, ok := chooseListing(nil, catalogKey); ok {
		t.Fatal("empty group selected a listing")
	}
}

func slotListing(pub, id, code string, at nostr.Timestamp) Listing {
	return Listing{
		AppID: "com.example.app",
		App: nostr.Event{
			Kind:   events.KindApp,
			PubKey: pub,
			ID:     id,
			Tags:   nostr.Tags{{"d", "com.example.app"}},
		},
		Release: nostr.Event{Kind: events.KindRelease, PubKey: pub, ID: id, CreatedAt: at},
		Assets: []nostr.Event{{
			Kind:   events.KindAsset,
			PubKey: pub,
			Tags: nostr.Tags{
				{"i", "com.example.app"},
				{"version_code", code},
			},
		}},
	}
}

func TestReleaseJoinsTheAppEventItNames(t *testing.T) {
	const appID = "com.example.app"
	catalogKey := strings.Repeat("11", 32)
	developer := strings.Repeat("22", 32)
	cert := strings.Repeat("cc", 32)
	triple := func(pub, n, code string, extra ...nostr.Tag) []nostr.Event {
		assetID := strings.Repeat(n+"3", 32)
		release := nostr.Event{ID: strings.Repeat(n+"2", 32), Kind: events.KindRelease, PubKey: pub, CreatedAt: 1, Tags: nostr.Tags{
			{"i", appID}, {"version", "1." + code}, {"d", appID + "@1." + code}, {"c", "main"}, {"e", assetID},
		}}
		release.Tags = append(release.Tags, extra...)
		return []nostr.Event{
			{ID: strings.Repeat(n+"1", 32), Kind: events.KindApp, PubKey: pub, CreatedAt: 1, Tags: nostr.Tags{
				{"d", appID}, {"name", "By " + n}, {"f", "android-arm64-v8a"},
			}},
			release,
			{ID: assetID, Kind: events.KindAsset, PubKey: pub, CreatedAt: 1, Tags: nostr.Tags{
				{"i", appID}, {"x", strings.Repeat(n+"4", 32)}, {"version", "1." + code},
				{"m", "application/vnd.android.package-archive"}, {"f", "android-arm64-v8a"},
				{"version_code", code}, {"apk_certificate_hash", cert},
			}},
		}
	}
	releaseOnly := func(evs []nostr.Event) []nostr.Event { return evs[1:] }
	dev := triple(developer, "d", "5")

	// The catalog key's release names the developer's app event.
	named := releaseOnly(triple(catalogKey, "c", "6", nostr.Tag{"a", "32267:" + developer + ":" + appID}))
	state := ResolveAt(append(slices.Clone(dev), named...), catalogKey, time.Now())
	if len(state.Listings) != 1 {
		t.Fatalf("listings %d, want 1", len(state.Listings))
	}
	got := state.Listings[0]
	if got.App.PubKey != developer || got.Release.PubKey != catalogKey || got.Assets[0].PubKey != catalogKey {
		t.Fatalf("app %s release %s asset %s, want the developer's app with the catalog key's release",
			got.App.PubKey[:4], got.Release.PubKey[:4], got.Assets[0].PubKey[:4])
	}

	// A lower catalog release does not win, and the developer's own triple does.
	low := releaseOnly(triple(catalogKey, "c", "4", nostr.Tag{"a", "32267:" + developer + ":" + appID}))
	state = ResolveAt(append(slices.Clone(dev), low...), catalogKey, time.Now())
	if len(state.Listings) != 1 || state.Listings[0].Release.PubKey != developer {
		t.Fatalf("a lower release won: %+v", state.Listings)
	}

	// A coordinate naming an app event that is gone joins the one that remains.
	stale := releaseOnly(triple(catalogKey, "c", "7", nostr.Tag{"a", "32267:" + strings.Repeat("99", 32) + ":" + appID}))
	state = ResolveAt(append(slices.Clone(dev), stale...), catalogKey, time.Now())
	if len(state.Listings) != 1 || state.Listings[0].Release.PubKey != catalogKey || state.Listings[0].App.PubKey != developer {
		t.Fatalf("a stale coordinate did not join the remaining app event: %+v", state.Listings)
	}

	// Without an `a` tag a release belongs only to its own signer's app event.
	bare := releaseOnly(triple(catalogKey, "c", "8"))
	state = ResolveAt(append(slices.Clone(dev), bare...), catalogKey, time.Now())
	if len(state.Listings) != 1 || state.Listings[0].Release.PubKey != developer {
		t.Fatalf("an unnamed release joined another key's app event: %+v", state.Listings)
	}
}
