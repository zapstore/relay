package relay

import (
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/relay/store"
	"github.com/zapstore/relay/pkg/search"
)

func TestSearchREQ(t *testing.T) {
	r := testRelay(t, newFakeBlossom())
	cert := strings.Repeat("cd", 32)
	developer := testPubkey(t)
	saveListing(t, r.store, catalogKey, "com.example.app", cert, "1", "2")
	saveListing(t, r.store, developer, "com.example.app", cert, "2", "2")
	laterRelease(t, r.store, "2")
	filter := nostr.Filter{Kinds: []int{events.KindApp}, Search: "example", Limit: 10}

	if _, err := r.query(t.Context(), nil, "s", nostr.Filters{filter}); !errors.Is(err, store.ErrUnsupportedREQ) {
		t.Fatalf("without an engine: %v", err)
	}

	loader := SearchListings(r.store, catalogKey, filepath.Join(t.TempDir(), "artifacts.db"))
	r.UseSearch(search.NewEngine(loader, nil, nil, slog.Default()))
	got, err := r.query(t.Context(), nil, "s", nostr.Filters{filter})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PubKey != developer {
		t.Fatalf("hits %+v, want the selected developer listing", got)
	}

	filter.Search = "example filter:source_available"
	if got, err := r.query(t.Context(), nil, "s", nostr.Filters{filter}); err != nil || len(got) != 0 {
		t.Fatalf("filter:source_available kept a listing without the fact: %d %v", len(got), err)
	}
	filter.Search = "example source available -filter:source_available"
	if got, err := r.query(t.Context(), nil, "s", nostr.Filters{filter}); err != nil || len(got) != 1 {
		t.Fatalf("-filter:source_available did not drop the phrase filter: %d %v", len(got), err)
	}

	hash := strings.Repeat("ab", 32)
	saveSlotEvent(t, r.store, nostr.Event{
		ID: strings.Repeat("c", 64), PubKey: developer, Kind: events.KindAsset,
		Tags: nostr.Tags{{"i", "com.example.app"}, {"x", hash}, {"f", "android-arm64-v8a"}},
	})
	found, err := r.query(t.Context(), nil, "h", nostr.Filters{{
		Kinds:  []int{events.KindAsset},
		Search: strings.ToUpper(hash),
		Limit:  1,
	}})
	if err != nil || len(found) != 1 || found[0].ID != strings.Repeat("c", 64) {
		t.Fatalf("hash search: %d %v", len(found), err)
	}

	filter.Search = "example"
	filter.Tags = nostr.TagMap{"f": []string{"android-x86_64"}}
	if got, err := r.query(t.Context(), nil, "s", nostr.Filters{filter}); err != nil || len(got) != 0 {
		t.Fatalf("#f did not filter before ranking: %d %v", len(got), err)
	}
}

func TestSearchMarksCatalogStackApps(t *testing.T) {
	r := testRelay(t, newFakeBlossom())
	cert := strings.Repeat("cd", 32)
	saveListing(t, r.store, catalogKey, "com.example.one", cert, "1", "1")
	saveListing(t, r.store, catalogKey, "com.example.two", cert, "2", "1")
	stack := func(id string, at nostr.Timestamp, appID string) {
		t.Helper()
		event := nostr.Event{
			ID: strings.Repeat(id, 64), PubKey: catalogKey, Kind: events.KindStack, CreatedAt: at,
			Tags: nostr.Tags{{"d", "picks"}, {"a", "32267:" + catalogKey + ":" + appID}, {"f", "android-arm64-v8a"}},
		}
		if _, err := r.store.Replace(t.Context(), &event); err != nil {
			t.Fatal(err)
		}
	}
	stack("e", 1, "com.example.one")
	artifactsPath := filepath.Join(t.TempDir(), "artifacts.db")
	engine := search.NewEngine(SearchListings(r.store, catalogKey, artifactsPath), SearchApp(r.store, catalogKey, artifactsPath), nil, slog.Default())
	r.UseSearch(engine)
	curated := func() map[string]bool {
		t.Helper()
		result, err := engine.Search(t.Context(), search.Request{Query: "com.example", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, hit := range result.Hits {
			out[hit.ID] = hit.Curated
		}
		return out
	}
	if got := curated(); !got["com.example.one"] || got["com.example.two"] {
		t.Fatalf("curated %v, want only com.example.one", got)
	}

	before, err := curatedApps(t.Context(), r.store, catalogKey)
	if err != nil {
		t.Fatal(err)
	}
	stack("f", 2, "com.example.two")
	r.syncCurated(t.Context(), before)
	if got := curated(); got["com.example.one"] || !got["com.example.two"] {
		t.Fatalf("after the stack changed, curated %v, want only com.example.two", got)
	}
}
