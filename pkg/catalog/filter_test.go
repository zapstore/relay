package catalog

import (
	"strings"
	"testing"
	"time"

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

func TestFilterKeepsCompleteListing(t *testing.T) {
	const (
		appID     = "com.timebinding.manhoodofhumanity"
		publisher = "aa9047325603dacd4f8142093567973566de3b1e20a89557b728c3be4c6a844b"
		signer    = "bb9047325603dacd4f8142093567973566de3b1e20a89557b728c3be4c6a844b"
		assetID   = "fe6469842079b0fc4f5ea0d30518c720f239324235a81a1359e2969dd2ac0546"
	)
	hash := strings.Repeat("ab", 32)
	cert := strings.Repeat("cd", 32)
	raw := []nostr.Event{
		{Kind: events.KindApp, PubKey: publisher, Tags: nostr.Tags{
			{"d", appID}, {"name", "Manhood"}, {"f", "android-arm64-v8a"},
		}},
		{Kind: events.KindRelease, PubKey: publisher, Tags: nostr.Tags{
			{"i", appID}, {"version", "1.0"}, {"d", appID + "@1.0"}, {"c", "main"}, {"e", assetID},
		}},
		{ID: assetID, Kind: events.KindAsset, PubKey: publisher, Tags: nostr.Tags{
			{"i", appID}, {"x", hash}, {"version", "1.0"},
			{"m", "application/vnd.android.package-archive"},
			{"f", "android-arm64-v8a"}, {"version_code", "1"},
			{"apk_certificate_hash", cert},
		}},
	}
	at := time.Unix(40, 0)
	if got := stateForBundle(raw, signer, "", at); len(got.Listings) != 0 {
		t.Fatalf("unfiltered listings = %d", len(got.Listings))
	}
	got := stateForBundle(raw, signer, "com.timeb", at)
	if len(got.Listings) != 1 || got.Listings[0].AppID != appID {
		t.Fatalf("filter listings = %+v", got.Listings)
	}
	if got := stateForBundle(raw, signer, "com.timebx", at); len(got.Listings) != 0 {
		t.Fatalf("unrelated filter listings = %d", len(got.Listings))
	}
}

func TestMatchingEmptyFilterKeepsTheCatalog(t *testing.T) {
	st := State{Listings: []Listing{{AppID: "com.example.maps"}, {AppID: "com.example.beacon"}}}
	got := st.matching("  ")
	if len(got.Listings) != 2 {
		t.Fatalf("listings = %d", len(got.Listings))
	}
}
