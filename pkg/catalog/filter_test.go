package catalog

import (
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

func TestOnlyMatchingDropsOtherApps(t *testing.T) {
	const author = "aa"
	next := State{Listings: []Listing{
		{AppID: "com.maps", App: nostr.Event{PubKey: author}},
		{AppID: "com.maps.unchanged", App: nostr.Event{PubKey: author}},
		{AppID: "com.other"},
	}}
	diff := Diff{
		Apps:       []string{"com.maps", "com.other"},
		AppDeletes: []string{"com.maps.gone", "com.other.gone"},
		Events: []nostr.Event{
			{ID: "map-app", Kind: 32267, Tags: nostr.Tags{{"d", "com.maps"}}},
			{ID: "other-app", Kind: 32267, Tags: nostr.Tags{{"d", "com.other"}}},
			{ID: "map-rel", Kind: 30063, Tags: nostr.Tags{{"i", "com.maps"}}},
			{ID: "prof", Kind: 0, PubKey: author},
			{ID: "stranger", Kind: 0, PubKey: "zz"},
			{ID: "stack", Kind: 30267},
		},
		Avatars: []string{author + ".webp", "zz.webp"},
		Coords:  []Coord{{Kind: 1, D: "x"}},
	}
	got := onlyMatching(diff, next, "maps")
	if len(got.Apps) != 1 || got.Apps[0] != "com.maps" {
		t.Fatalf("apps %v", got.Apps)
	}
	if len(got.AppDeletes) != 1 || got.AppDeletes[0] != "com.maps.gone" {
		t.Fatalf("deletes %v", got.AppDeletes)
	}
	if len(got.Coords) != 0 {
		t.Fatalf("coords %v", got.Coords)
	}
	if len(got.Avatars) != 1 || got.Avatars[0] != author+".webp" {
		t.Fatalf("avatars %v", got.Avatars)
	}
	ids := map[string]struct{}{}
	for _, event := range got.Events {
		ids[event.ID] = struct{}{}
	}
	for _, id := range []string{"map-app", "map-rel", "prof"} {
		if _, ok := ids[id]; !ok {
			t.Fatalf("missing %s in %v", id, ids)
		}
	}
	for _, id := range []string{"other-app", "stranger", "stack"} {
		if _, ok := ids[id]; ok {
			t.Fatalf("kept %s", id)
		}
	}
}
