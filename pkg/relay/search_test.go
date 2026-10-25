package relay

import (
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	filter := nostr.Filter{Kinds: []int{events.KindApp}, Search: "example", Limit: 10}

	if _, err := r.query(t.Context(), nil, "s", nostr.Filters{filter}); !errors.Is(err, store.ErrUnsupportedREQ) {
		t.Fatalf("without an engine: %v", err)
	}

	loader := SearchListings(r.store, catalogKey, filepath.Join(t.TempDir(), "artifacts.db"))
	r.UseSearch(search.NewEngine(loader, nil, time.Minute, slog.Default()))
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
