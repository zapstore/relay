package relay

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/search"
)

func TestLiveRead(t *testing.T) {
	r := testRelay(t, newFakeBlossom())
	cert := strings.Repeat("cd", 32)
	developer := testPubkey(t)
	saveListing(t, r.store, catalogKey, "com.example.app", cert, "1", "2")
	saveListing(t, r.store, developer, "com.example.app", cert, "2", "2")
	saveListing(t, r.store, catalogKey, "com.example.solo", cert, "3", "1")

	// Two assets with the same version_code; the release names the higher id first.
	low, high := strings.Repeat("4a", 32), strings.Repeat("4b", 32)
	saveSlotEvent(t, r.store, nostr.Event{
		ID: strings.Repeat("40", 32), PubKey: catalogKey, Kind: events.KindApp,
		Tags: nostr.Tags{{"d", "com.example.pair"}, {"name", "Pair"}, {"f", "android-arm64-v8a"}},
	})
	saveSlotEvent(t, r.store, nostr.Event{
		ID: strings.Repeat("41", 32), PubKey: catalogKey, Kind: events.KindRelease,
		Tags: nostr.Tags{{"i", "com.example.pair"}, {"version", "1.0.5"}, {"d", "com.example.pair@1.0.5"}, {"c", "main"}, {"e", high}, {"e", low}},
	})
	for _, id := range []string{low, high} {
		saveSlotEvent(t, r.store, nostr.Event{
			ID: id, PubKey: catalogKey, Kind: events.KindAsset,
			Tags: nostr.Tags{
				{"i", "com.example.pair"}, {"x", id}, {"version", "1.0.5"}, {"f", "android-arm64-v8a"},
				{"version_code", "5"}, {"apk_certificate_hash", cert},
			},
		})
	}
	if err := SelectAll(t.Context(), r.store, catalogKey); err != nil {
		t.Fatal(err)
	}

	read := func(t *testing.T, filter nostr.Filter) []nostr.Event {
		t.Helper()
		filter.Kinds = []int{events.KindApp}
		got, err := r.query(t.Context(), nil, "live", nostr.Filters{filter})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	ids := func(got []nostr.Event) []string {
		var out []string
		for _, event := range got {
			out = append(out, event.ID[:2])
		}
		return out
	}

	t.Run("listing:all is every app in app-id order", func(t *testing.T) {
		got := ids(read(t, nostr.Filter{Search: "listing:all"}))
		want := []string{"20", "21", "2a", "40", "41", "4b", "4a", "30", "31", "3a"}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("events %v, want %v", got, want)
		}
	})
	t.Run("listing:asset sends the app and its lowest-id top asset", func(t *testing.T) {
		got := ids(read(t, nostr.Filter{Search: "listing:asset filter:offline_capable", Tags: nostr.TagMap{"d": {"com.example.pair"}}}))
		if strings.Join(got, " ") != "40 4a" {
			t.Fatalf("events %v", got)
		}
	})
	t.Run("both tokens answer listing:all", func(t *testing.T) {
		got := read(t, nostr.Filter{Search: "listing:asset listing:all", Tags: nostr.TagMap{"d": {"com.example.app"}}})
		if len(got) != 3 || got[0].PubKey != developer {
			t.Fatalf("events %v, want the developer triple", ids(got))
		}
	})
	t.Run("limit caps apps", func(t *testing.T) {
		got := read(t, nostr.Filter{Search: "listing:asset", Limit: 2})
		if strings.Join(ids(got), " ") != "20 2a 40 4a" {
			t.Fatalf("events %v", ids(got))
		}
	})
	t.Run("budget leaves out an app that would not fit", func(t *testing.T) {
		got, err := r.liveREQ(t.Context(), nostr.Filter{Kinds: []int{events.KindApp}, Search: "listing:all"}, false, 5)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(ids(got), " ") != "20 21 2a" {
			t.Fatalf("events %v, want only the first app", ids(got))
		}
	})
	t.Run("query text makes it a search", func(t *testing.T) {
		loader := SearchListings(r.store, filepath.Join(t.TempDir(), "artifacts.db"))
		r.UseSearch(search.NewEngine(loader, nil, nil, slog.Default()))
		defer r.UseSearch(nil)
		got := read(t, nostr.Filter{Search: "pair listing:all", Limit: 10})
		if len(got) != 1 || got[0].ID[:2] != "40" {
			t.Fatalf("events %v, want the ranked Pair listing", ids(got))
		}
	})
}

func TestLiveReadTokens(t *testing.T) {
	for search, want := range map[string][2]bool{
		"listing:all":                           {false, true},
		"LISTING:ASSET":                         {true, true},
		"listing:asset listing:all":             {false, true},
		"listing:asset -filter:offline_capable": {true, true},
		"maps listing:all":                      {false, false},
		"filter:offline_capable":                {false, false},
		"listing:everything":                    {false, false},
	} {
		assetOnly, ok := liveRead(nostr.Filters{{Search: search}})
		if assetOnly != want[0] || ok != want[1] {
			t.Errorf("%q: assetOnly %v ok %v, want %v", search, assetOnly, ok, want)
		}
	}
}
