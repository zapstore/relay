package catalog

import (
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestChooseListingSlots(t *testing.T) {
	const catalogKey = "cc"
	indexer := slotListing(catalogKey, "10", "2")
	developer := slotListing("dd", "11", "2")

	chosen, ok := chooseListing([]Listing{indexer, developer}, catalogKey)
	if !ok || chosen.App.PubKey != "dd" {
		t.Fatalf("tie chose %q ok=%v", chosen.App.PubKey, ok)
	}
	chosen, _ = chooseListing([]Listing{developer, indexer}, catalogKey)
	if chosen.App.PubKey != "dd" {
		t.Fatalf("tie in reverse order chose %q", chosen.App.PubKey)
	}

	newerIndexer := slotListing(catalogKey, "13", "3")
	chosen, _ = chooseListing([]Listing{newerIndexer, developer}, catalogKey)
	if chosen.App.PubKey != catalogKey {
		t.Fatalf("higher indexer chose %q", chosen.App.PubKey)
	}

	newerDeveloper := slotListing("dd", "14", "9")
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

func slotListing(pub, id, code string) Listing {
	return Listing{
		AppID: "com.example.app",
		App: nostr.Event{
			Kind:   events.KindApp,
			PubKey: pub,
			ID:     id,
			Tags:   nostr.Tags{{"d", "com.example.app"}},
		},
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
