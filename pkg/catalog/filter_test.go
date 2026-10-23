package catalog

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/relay/store"
)

func listingEventsFor(appID, pub string, n int) []nostr.Event {
	assetID := strings.Repeat(string(rune('a'+n)), 64)
	hash := strings.Repeat(string(rune('0'+n)), 64)
	return []nostr.Event{
		{ID: strings.Repeat(string(rune('a'+n)), 63) + "1", Kind: events.KindApp, PubKey: pub, CreatedAt: 1, Tags: nostr.Tags{
			{"d", appID}, {"name", appID}, {"f", "android-arm64-v8a"},
		}},
		{ID: strings.Repeat(string(rune('a'+n)), 63) + "2", Kind: events.KindRelease, PubKey: pub, CreatedAt: 1, Tags: nostr.Tags{
			{"i", appID}, {"version", "1.0"}, {"d", appID + "@1.0"}, {"c", "main"}, {"e", assetID},
		}},
		{ID: assetID, Kind: events.KindAsset, PubKey: pub, CreatedAt: 1, Tags: nostr.Tags{
			{"i", appID}, {"x", hash}, {"version", "1.0"},
			{"m", "application/vnd.android.package-archive"},
			{"f", "android-arm64-v8a"}, {"version_code", "1"},
			{"apk_certificate_hash", hash},
		}},
	}
}

func TestFilteredSealWritesTheWholeCatalog(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "relay.db")
	db, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	var all []nostr.Event
	all = append(all, listingEventsFor("com.example.maps", signer.PubKey, 1)...)
	all = append(all, listingEventsFor("com.example.beacon", signer.PubKey, 2)...)
	for i := range all {
		if _, err := db.Save(t.Context(), &all[i]); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	var enriched []string
	enrich := func(_ context.Context, ids []string) error {
		enriched = append(enriched, ids...)
		return nil
	}
	n, err := Bundle(t.Context(), dir, dbPath, "maps", signer, nil, enrich)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(enriched, []string{"com.example.maps"}) {
		t.Fatalf("enriched %v", enriched)
	}
	st, err := ReadSnap(SnapPath(dir, n))
	if err != nil {
		t.Fatal(err)
	}
	if got := appIDs(st); !slices.Equal(got, []string{"com.example.beacon", "com.example.maps"}) {
		t.Fatalf("snapshot apps %v", got)
	}
	if _, err := Bundle(t.Context(), dir, dbPath, "nothing", signer, nil, enrich); err == nil {
		t.Fatal("a filter that matches nothing must fail")
	}
}

func TestMatchingApps(t *testing.T) {
	st := State{Listings: []Listing{{AppID: "com.example.maps"}, {AppID: "com.example.beacon"}}}
	if got := matchingApps(st, "maps"); !slices.Equal(got, []string{"com.example.maps"}) {
		t.Fatalf("matching %v", got)
	}
}
