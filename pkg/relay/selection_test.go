package relay

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/relay/store"
)

func TestSelectSlots(t *testing.T) {
	const appID = "com.example.app"
	cert := strings.Repeat("cd", 32)
	catalog := testPubkey(t)
	developer := testPubkey(t)
	solo := "com.example.solo"

	setup := func(t *testing.T, catalogCode string) store.T {
		db := newSlotDB(t)
		saveListing(t, db, catalog, appID, cert, "1", catalogCode)
		saveListing(t, db, developer, appID, cert, "2", "2")
		saveListing(t, db, catalog, solo, cert, "3", "1")
		return db
	}
	everything := nostr.Filters{
		{Kinds: []int{events.KindApp}, Tags: nostr.TagMap{"d": []string{appID, solo}}, Limit: 100},
		{Kinds: []int{events.KindRelease, events.KindAsset}, Tags: nostr.TagMap{"i": []string{appID, solo}}, Limit: 100},
	}

	for name, tc := range map[string]struct {
		catalogCode string
		filters     nostr.Filters
		want        map[string]int
	}{
		"developer wins a tie":     {"2", everything, map[string]int{developer: 3, catalog: 3}},
		"higher version_code wins": {"3", everything, map[string]int{catalog: 6}},
		"authors keep their own listing": {"2", nostr.Filters{
			{Kinds: []int{events.KindApp, events.KindRelease, events.KindAsset}, Authors: []string{catalog}, Limit: 100},
		}, map[string]int{catalog: 6}},
	} {
		t.Run(name, func(t *testing.T) {
			db := setup(t, tc.catalogCode)
			for _, app := range []string{appID, solo} {
				if err := selectApp(context.Background(), db, catalog, app); err != nil {
					t.Fatal(err)
				}
			}
			kept := querySlots(t, db, tc.filters)
			got := map[string]int{}
			for _, event := range kept {
				got[event.PubKey]++
				if app, _ := slotAppID(event); app == solo && event.PubKey != catalog {
					t.Errorf("solo app event from %s", event.PubKey)
				}
			}
			total := 0
			for pubkey, n := range tc.want {
				total += n
				if got[pubkey] != n {
					t.Errorf("%s: %d events, want %d (all %v)", pubkey[:8], got[pubkey], n, got)
				}
			}
			if len(kept) != total {
				t.Errorf("kept %d events, want %d", len(kept), total)
			}
		})
	}
}

func TestSelectHiddenAfterDelete(t *testing.T) {
	const appID = "com.example.app"
	cert := strings.Repeat("cd", 32)
	catalog := testPubkey(t)
	developer := testPubkey(t)
	ctx := context.Background()
	filter := nostr.Filters{{Kinds: []int{events.KindApp}, Tags: nostr.TagMap{"d": []string{appID}}, Limit: 10}}
	signers := func(t *testing.T, db store.T) []string {
		t.Helper()
		var out []string
		for _, event := range querySlots(t, db, filter) {
			out = append(out, event.PubKey)
		}
		return out
	}

	db := newSlotDB(t)
	saveListing(t, db, catalog, appID, cert, "1", "3")
	if err := selectApp(ctx, db, catalog, appID); err != nil {
		t.Fatal(err)
	}
	if n := hiddenEvents(t, db); n != 0 {
		t.Fatalf("one slot: %d hidden events, want 0", n)
	}
	saveListing(t, db, developer, appID, cert, "2", "2")
	if err := selectApp(ctx, db, catalog, appID); err != nil {
		t.Fatal(err)
	}
	if n := hiddenEvents(t, db); n != 3 {
		t.Fatalf("two slots: %d hidden events, want the developer's 3", n)
	}
	if got := signers(t, db); len(got) != 1 || got[0] != catalog {
		t.Fatalf("before: %v, want the catalog key", got)
	}
	if _, err := db.Delete(ctx, nostr.Filter{Authors: []string{catalog}}); err != nil {
		t.Fatal(err)
	}
	if _, err := selectHidden(ctx, db, catalog); err != nil {
		t.Fatal(err)
	}
	if n := hiddenEvents(t, db); n != 0 {
		t.Fatalf("after deleting the selected slot: %d hidden events, want 0", n)
	}
	if got := signers(t, db); len(got) != 1 || got[0] != developer {
		t.Fatalf("after: %v, want the developer", got)
	}
}

func TestPromotionHidesTheOtherSlot(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	key := newTestKey(t)
	r.readAPK = readChains(t, map[string]signing{"dev-apk": {key.cert}})
	apps := nostr.Filters{{Kinds: []int{events.KindApp}, Tags: nostr.TagMap{"d": []string{testApp}}, Limit: 10}}
	visible := func(t *testing.T) []string {
		t.Helper()
		got, err := r.query(t.Context(), nil, "q", apps)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, event := range got {
			out = append(out, event.PubKey)
		}
		return out
	}

	catApp, catRelease, catAsset := testTriple("c", catalogKey, blobs.put("cat-apk"), key.hash, 1)
	for _, event := range []*nostr.Event{catApp, catAsset, catRelease} {
		r.holdListing(t.Context(), event)
	}
	drainReads(t, r)
	if !slotHas(t, r.store, catRelease.ID) {
		t.Fatal("the catalog listing was not promoted")
	}
	if n := hiddenEvents(t, r.store); n != 0 {
		t.Fatalf("one slot: %d hidden events", n)
	}

	devApp, devRelease, devAsset := testTriple("b", ownerKey, blobs.put("dev-apk"), key.hash, 2)
	for _, event := range []*nostr.Event{devApp, devAsset, devRelease} {
		r.holdListing(t.Context(), event)
	}
	drainReads(t, r)
	if res := r.persist(nil, key.proof(t, "4", ownerKey, "")); !res.ok {
		t.Fatalf("proof: %+v", res)
	}
	if !slotHas(t, r.store, devRelease.ID) {
		t.Fatal("the developer listing was not promoted")
	}
	if got := visible(t); len(got) != 1 || got[0] != ownerKey {
		t.Fatalf("two slots: apps from %v, want the developer's", got)
	}
	if n := hiddenEvents(t, r.store); n != 3 {
		t.Fatalf("two slots: %d hidden events, want the catalog's 3", n)
	}

	revocation := &nostr.Event{
		ID: strings.Repeat("5", 64), PubKey: ownerKey, Kind: events.KindIdentityProof,
		CreatedAt: nostr.Now() + 10, Tags: nostr.Tags{{"d", key.hash}, {"revoked"}},
	}
	if res := r.persist(nil, revocation); !res.ok {
		t.Fatalf("revocation: %+v", res)
	}
	if got := visible(t); len(got) != 1 || got[0] != catalogKey {
		t.Fatalf("after revocation: apps from %v, want the catalog key's", got)
	}
	if n := hiddenEvents(t, r.store); n != 0 {
		t.Fatalf("after revocation: %d hidden events", n)
	}
}

func hiddenEvents(t *testing.T, db store.T) int {
	t.Helper()
	var n int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM events WHERE hidden = 1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func querySlots(t *testing.T, db store.T, filters nostr.Filters) []nostr.Event {
	t.Helper()
	kept, err := db.QueryVisible(context.Background(), filters...)
	if err != nil {
		t.Fatal(err)
	}
	return kept
}

// saveListing stores a complete main-channel listing: app, release, and one arm64 asset.
func saveListing(t *testing.T, db store.T, pubkey, appID, cert, seed, versionCode string) {
	t.Helper()
	assetID := strings.Repeat(seed+"a", 32)
	saveSlotEvent(t, db, nostr.Event{
		ID:     strings.Repeat(seed+"0", 32),
		PubKey: pubkey,
		Kind:   events.KindApp,
		Tags:   nostr.Tags{{"d", appID}, {"name", "Example"}, {"f", "android-arm64-v8a"}},
	})
	saveSlotEvent(t, db, nostr.Event{
		ID:     strings.Repeat(seed+"1", 32),
		PubKey: pubkey,
		Kind:   events.KindRelease,
		Tags: nostr.Tags{
			{"i", appID},
			{"version", "1.0." + versionCode},
			{"d", fmt.Sprintf("%s@1.0.%s", appID, versionCode)},
			{"c", "main"},
			{"e", assetID},
		},
	})
	saveSlotEvent(t, db, nostr.Event{
		ID:     assetID,
		PubKey: pubkey,
		Kind:   events.KindAsset,
		Tags: nostr.Tags{
			{"i", appID},
			{"x", strings.Repeat(seed+"b", 32)},
			{"version", "1.0." + versionCode},
			{"m", "application/vnd.android.package-archive"},
			{"f", "android-arm64-v8a"},
			{"version_code", versionCode},
			{"apk_certificate_hash", cert},
		},
	})
}
