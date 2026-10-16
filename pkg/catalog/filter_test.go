package catalog

import (
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestMatchingKeepsEventsForTheFilter(t *testing.T) {
	cert := strings.Repeat("ab", 32)
	other := strings.Repeat("cd", 32)
	pub := strings.Repeat("11", 32)
	otherPub := strings.Repeat("22", 32)
	maps := Listing{
		AppID:   "com.example.maps",
		App:     nostr.Event{ID: "maps-app", Kind: events.KindApp, PubKey: pub},
		Release: nostr.Event{ID: "maps-rel", Kind: events.KindRelease},
		Assets: []nostr.Event{{
			ID:   "maps-asset",
			Kind: events.KindAsset,
			Tags: nostr.Tags{{"apk_certificate_hash", cert}},
		}},
	}
	beacon := Listing{
		AppID:   "com.example.beacon",
		App:     nostr.Event{ID: "beacon-app", Kind: events.KindApp, PubKey: otherPub},
		Release: nostr.Event{ID: "beacon-rel", Kind: events.KindRelease},
		Assets: []nostr.Event{{
			ID:   "beacon-asset",
			Kind: events.KindAsset,
			Tags: nostr.Tags{{"apk_certificate_hash", other}},
		}},
	}
	stackBoth := nostr.Event{
		ID: "stack-both", Kind: events.KindStack, Tags: nostr.Tags{
			{"d", "both"}, {"f", preferredABI},
			{"a", "32267:" + pub + ":com.example.maps"},
			{"a", "32267:" + otherPub + ":com.example.beacon"},
		},
	}
	stackMaps := nostr.Event{
		ID: "stack-maps", Kind: events.KindStack, Tags: nostr.Tags{
			{"d", "maps"}, {"f", preferredABI},
			{"a", "32267:" + pub + ":com.example.maps"},
		},
	}
	relays := nostr.Event{ID: "relays", Kind: events.KindAppRelays, PubKey: pub}
	got := (State{
		Listings: []Listing{maps, beacon},
		Proofs: []Proof{
			{Pubkey: pub, D: strings.ToUpper(cert), Event: nostr.Event{ID: "proof-maps"}},
			{Pubkey: otherPub, D: other, Event: nostr.Event{ID: "proof-beacon"}},
		},
		Profiles: []Profile{
			{Pubkey: pub, Event: nostr.Event{ID: "profile-maps"}},
			{Pubkey: otherPub, Event: nostr.Event{ID: "profile-beacon"}},
		},
		Stacks: []Stack{{Event: stackBoth}, {Event: stackMaps}},
		Relays: []Proof{{Event: relays}},
	}).matching("maps")

	ids := map[string]bool{}
	for _, event := range got.Events() {
		ids[event.ID] = true
	}
	for _, id := range []string{"maps-app", "maps-rel", "maps-asset", "proof-maps", "profile-maps", "stack-maps", "relays"} {
		if !ids[id] {
			t.Fatalf("missing %s in %v", id, ids)
		}
	}
	for _, id := range []string{"beacon-app", "beacon-rel", "beacon-asset", "proof-beacon", "profile-beacon", "stack-both"} {
		if ids[id] {
			t.Fatalf("kept %s", id)
		}
	}
}

func TestMatchingEmptyFilterKeepsTheCatalog(t *testing.T) {
	st := State{Listings: []Listing{{AppID: "com.example.maps"}, {AppID: "com.example.beacon"}}}
	got := st.matching("  ")
	if len(got.Listings) != 2 {
		t.Fatalf("listings = %d", len(got.Listings))
	}
}
