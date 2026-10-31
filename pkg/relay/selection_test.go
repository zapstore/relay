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

func TestSelectedListings(t *testing.T) {
	const appID = "com.example.app"
	cert := strings.Repeat("cd", 32)
	catalog := testPubkey(t)
	developer := testPubkey(t)
	solo := "com.example.solo"

	for name, tc := range map[string]struct {
		catalogCode, developerCode string
		catalogAt, developerAt     nostr.Timestamp
		want                       string
	}{
		"higher version_code wins":        {"3", "2", 1, 9, catalog},
		"the developer can have it":       {"2", "5", 9, 1, developer},
		"a tie goes to the later release": {"2", "2", 5, 9, developer},
		"a tie in the other direction":    {"2", "2", 9, 5, catalog},
	} {
		t.Run(name, func(t *testing.T) {
			db := newSlotDB(t)
			saveListing(t, db, catalog, appID, cert, "1", tc.catalogCode)
			saveListing(t, db, developer, appID, cert, "2", tc.developerCode)
			saveListing(t, db, catalog, solo, cert, "3", "1")
			for id, at := range map[string]nostr.Timestamp{strings.Repeat("11", 32): tc.catalogAt, strings.Repeat("21", 32): tc.developerAt} {
				if _, err := db.DB.Exec(`UPDATE events SET created_at = ? WHERE id = ?`, at, id); err != nil {
					t.Fatal(err)
				}
			}
			got, err := selectedListings(context.Background(), db, catalog, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got[0].AppID != appID || got[1].AppID != solo {
				t.Fatalf("listings %+v, want %s then %s", got, appID, solo)
			}
			if got[0].App.PubKey != tc.want || got[0].Release.PubKey != tc.want {
				t.Fatalf("selected %s, want %s", got[0].App.PubKey[:8], tc.want[:8])
			}
			if got[1].App.PubKey != catalog {
				t.Fatal("a one-listing app lost its listing")
			}
		})
	}
}

func TestSelectedListingsLimitAndFilter(t *testing.T) {
	cert := strings.Repeat("cd", 32)
	catalog := testPubkey(t)
	db := newSlotDB(t)
	saveListing(t, db, catalog, "com.example.a", cert, "1", "1")
	saveListing(t, db, catalog, "com.example.b", cert, "2", "1")
	saveListing(t, db, catalog, "com.example.c", cert, "3", "1")
	got, err := selectedListings(context.Background(), db, catalog, nil, 2)
	if err != nil || len(got) != 2 || got[1].AppID != "com.example.b" {
		t.Fatalf("limit 2: %v %v", got, err)
	}
	got, err = selectedListings(context.Background(), db, catalog, []string{"com.example.c"}, 0)
	if err != nil || len(got) != 1 || got[0].AppID != "com.example.c" {
		t.Fatalf("filter: %v %v", got, err)
	}
}

func TestPromotionReplacesTheListingAndKeepsOlderReleases(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	key := newTestKey(t)
	r.readAPK = readChains(t, map[string]signing{"dev-apk": {key.cert}})
	apps := nostr.Filters{{Kinds: []int{events.KindApp}, Tags: nostr.TagMap{"d": []string{testApp}}, Limit: 10}}
	served := func(t *testing.T) []string {
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
	current := func(t *testing.T) (app, release string) {
		t.Helper()
		got, err := selectedListings(t.Context(), r.store, catalogKey, []string{testApp}, 0)
		if err != nil || len(got) != 1 {
			t.Fatalf("listings %v %v", got, err)
		}
		return got[0].App.PubKey, got[0].Release.PubKey
	}

	catApp, catRelease, catAsset := testTriple("c", catalogKey, blobs.put("cat-apk"), key.hash, 1)
	for _, event := range []*nostr.Event{catApp, catAsset, catRelease} {
		r.holdListing(t.Context(), event)
	}
	drainReads(t, r)
	if !slotHas(t, r.store, catRelease.ID) {
		t.Fatal("the catalog listing was not promoted")
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
	if got := served(t); len(got) != 1 || got[0] != ownerKey {
		t.Fatalf("apps from %v, want only the developer's", got)
	}
	if !slotHas(t, r.store, catRelease.ID) || !slotHas(t, r.store, catAsset.ID) {
		t.Fatal("the older release was deleted")
	}
	if app, release := current(t); app != ownerKey || release != ownerKey {
		t.Fatalf("current listing from %s and %s", app, release)
	}

	// A lower release dated after a higher one is a downgrade and is refused,
	// whoever sends it.
	lowAsset := testAsset("l", catalogKey, blobs.put("low"), key.hash, 1)
	lowRelease := testRelease("m", catalogKey, "0.9.0", "main", lowAsset.ID)
	lowRelease.Tags[0] = nostr.Tag{"a", "32267:" + ownerKey + ":" + testApp}
	r.holdListing(t.Context(), lowAsset)
	if res := r.holdListing(t.Context(), lowRelease); res.ok || !strings.Contains(res.msg, "lower than 2") {
		t.Fatalf("lower release: %+v", res)
	}

	// The indexer sends only a release, naming the developer's app event.
	r.readAPK = readChains(t, map[string]signing{"dev-apk": {key.cert}, "cat-apk3": {key.cert}})
	topAsset := testAsset("e", catalogKey, blobs.put("cat-apk3"), key.hash, 3)
	topRelease := testRelease("f", catalogKey, "1.0.3", "main", topAsset.ID)
	topRelease.Tags[0] = nostr.Tag{"a", "32267:" + ownerKey + ":" + testApp}
	r.holdListing(t.Context(), topAsset)
	if res := r.holdListing(t.Context(), topRelease); !res.ok {
		t.Fatalf("release naming the developer's app event: %+v", res)
	}
	if !slotHas(t, r.store, topRelease.ID) || !slotHas(t, r.store, devApp.ID) {
		t.Fatal("the indexer's release was not stored, or the developer's app event went")
	}
	if app, release := current(t); app != ownerKey || release != catalogKey {
		t.Fatalf("current listing from %s and %s, want the developer's app event with the indexer's release", app, release)
	}

	// A release naming an app event of a key that is not the developer is refused.
	badAsset := testAsset("g", catalogKey, blobs.put("bad"), key.hash, 4)
	badRelease := testRelease("h", catalogKey, "1.0.4", "main", badAsset.ID)
	badRelease.Tags[0] = nostr.Tag{"a", "32267:" + strings.Repeat("9", 64) + ":" + testApp}
	r.holdListing(t.Context(), badAsset)
	if res := r.holdListing(t.Context(), badRelease); res.ok || slotHas(t, r.store, badRelease.ID) {
		t.Fatalf("release naming a stranger's app event: %+v", res)
	}

	revocation := &nostr.Event{
		ID: strings.Repeat("5", 64), PubKey: ownerKey, Kind: events.KindIdentityProof,
		CreatedAt: nostr.Now() + 10, Tags: nostr.Tags{{"d", key.hash}, {"revoked"}},
	}
	if res := r.persist(nil, revocation); !res.ok {
		t.Fatalf("revocation: %+v", res)
	}
	if !slotHas(t, r.store, devRelease.ID) {
		t.Fatal("a revocation deleted the developer's listing")
	}
}

func TestLoneAppEditNeedsAReleaseAndTheDeveloper(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	key := newTestKey(t)
	r.readAPK = readChains(t, map[string]signing{"cat-apk": {key.cert}})

	edit := &nostr.Event{
		ID: strings.Repeat("e", 64), PubKey: ownerKey, Kind: events.KindApp, CreatedAt: 20,
		Tags: nostr.Tags{{"d", testApp}, {"name", "Edited"}, {"f", "android-arm64-v8a"}},
	}
	r.holdListing(t.Context(), edit)
	if slotHas(t, r.store, edit.ID) {
		t.Fatal("an app event was stored for an app with no release")
	}

	catApp, catRelease, catAsset := testTriple("c", catalogKey, blobs.put("cat-apk"), key.hash, 1)
	for _, event := range []*nostr.Event{catApp, catAsset, catRelease} {
		r.holdListing(t.Context(), event)
	}
	drainReads(t, r)
	if !slotHas(t, r.store, catRelease.ID) {
		t.Fatal("the catalog listing was not promoted")
	}

	// Still pending: the signer holds no proof.
	r.holdListing(t.Context(), edit)
	if slotHas(t, r.store, edit.ID) {
		t.Fatal("an app event from a key with no proof was stored")
	}

	saveSlotEvent(t, r.store, *key.proof(t, "4", ownerKey, ""))
	if res := r.holdListing(t.Context(), edit); !res.ok || !res.broadcast {
		t.Fatalf("edit: %+v", res)
	}
	if !slotHas(t, r.store, edit.ID) || slotHas(t, r.store, catApp.ID) {
		t.Fatal("the developer's app event did not replace the catalog's")
	}
	if !slotHas(t, r.store, catRelease.ID) {
		t.Fatal("the release was deleted by an edit of the app event")
	}
}

// laterRelease makes the release saveListing stored with seed the latest.
func laterRelease(t *testing.T, db store.T, seed string) {
	t.Helper()
	if _, err := db.DB.Exec(`UPDATE events SET created_at = 100 WHERE id = ?`, strings.Repeat(seed+"1", 32)); err != nil {
		t.Fatal(err)
	}
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

func TestCatalogKeepsItsOlderReleases(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	key := newTestKey(t)
	r.readAPK = readChains(t, map[string]signing{"cat-apk": {key.cert}, "cat-apk2": {key.cert}})

	app1, release1, asset1 := testTriple("c", catalogKey, blobs.put("cat-apk"), key.hash, 1)
	for _, event := range []*nostr.Event{app1, asset1, release1} {
		r.holdListing(t.Context(), event)
	}
	drainReads(t, r)
	app2, release2, asset2 := testTriple("d", catalogKey, blobs.put("cat-apk2"), key.hash, 2)
	release2.Tags[3] = nostr.Tag{"d", testApp + "@2"}
	release2.CreatedAt = 20
	for _, event := range []*nostr.Event{app2, asset2, release2} {
		r.holdListing(t.Context(), event)
	}
	drainReads(t, r)
	if !slotHas(t, r.store, release2.ID) {
		t.Fatal("the newer release was not stored")
	}
	if !slotHas(t, r.store, release1.ID) || !slotHas(t, r.store, asset1.ID) {
		t.Fatal("the older release of the catalog key was deleted")
	}
}

func TestCatalogDefersToTheDevelopersAppEvent(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	key := newTestKey(t)
	r.readAPK = readChains(t, map[string]signing{"cat-apk": {key.cert}, "dev-apk": {key.cert}, "cat-apk3": {key.cert}})

	catApp, catRelease, catAsset := testTriple("c", catalogKey, blobs.put("cat-apk"), key.hash, 1)
	catRelease.CreatedAt = 5
	for _, event := range []*nostr.Event{catApp, catAsset, catRelease} {
		r.holdListing(t.Context(), event)
	}
	drainReads(t, r)
	devApp, devRelease, devAsset := testTriple("b", ownerKey, blobs.put("dev-apk"), key.hash, 2)
	for _, event := range []*nostr.Event{devApp, devAsset, devRelease} {
		r.holdListing(t.Context(), event)
	}
	drainReads(t, r)
	if res := r.persist(nil, key.proof(t, "4", ownerKey, "")); !res.ok {
		t.Fatalf("proof: %+v", res)
	}
	if !slotHas(t, r.store, devRelease.ID) || slotHas(t, r.store, catApp.ID) {
		t.Fatal("the developer listing was not promoted over the catalog's")
	}

	// The indexer finds a forge release above the developer's. A whole
	// listing would replace the developer's kind 32267, so the relay refuses
	// it and keeps what it holds, including the catalog's assets only when a
	// release names them.
	topApp, topRelease, topAsset := testTriple("e", catalogKey, blobs.put("cat-apk3"), key.hash, 3)
	topRelease.Tags[2] = nostr.Tag{"version", "1.0.3"}
	topRelease.Tags[3] = nostr.Tag{"d", testApp + "@1.0.3"}
	topRelease.CreatedAt = 30
	if res := r.holdListing(t.Context(), topApp); res.ok || !strings.Contains(res.msg, "developer's app event") {
		t.Fatalf("catalog app event: %+v", res)
	}
	r.holdListing(t.Context(), topAsset)
	if res := r.holdListing(t.Context(), topRelease); res.ok || !strings.Contains(res.msg, "names it in its a tag") {
		t.Fatalf("catalog release naming its own app event: %+v", res)
	}
	for name, id := range map[string]string{"app": topApp.ID, "release": topRelease.ID, "asset": topAsset.ID} {
		if slotHas(t, r.store, id) {
			t.Fatalf("the refused catalog %s was stored", name)
		}
		if kept, err := pendingHas(t.Context(), r.store, id); err != nil || kept {
			t.Fatalf("the refused catalog %s is still pending: %v %v", name, kept, err)
		}
	}
	if !slotHas(t, r.store, devApp.ID) || !slotHas(t, r.store, devRelease.ID) {
		t.Fatal("the developer's listing changed")
	}

	// A release that names the developer's app event is accepted.
	nameAsset := testAsset("f", catalogKey, blobs.put("cat-apk3"), key.hash, 3)
	nameRelease := testRelease("g", catalogKey, "1.0.3", "main", nameAsset.ID)
	nameRelease.Tags[0] = nostr.Tag{"a", "32267:" + ownerKey + ":" + testApp}
	nameRelease.CreatedAt = 30
	r.holdListing(t.Context(), nameAsset)
	if res := r.holdListing(t.Context(), nameRelease); !res.ok {
		t.Fatalf("release naming the developer's app event: %+v", res)
	}
	if !slotHas(t, r.store, devApp.ID) {
		t.Fatal("the developer's app event went")
	}
	got, err := selectedListings(t.Context(), r.store, catalogKey, []string{testApp}, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("listings %v %v", got, err)
	}
	if got[0].App.PubKey != ownerKey || got[0].Release.ID != nameRelease.ID {
		t.Fatalf("current listing from %s, release %s", got[0].App.PubKey, got[0].Release.ID)
	}
}

func TestCatalogReplacesAnAppEventThatNoDeveloperHolds(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	key := newTestKey(t)
	r.readAPK = readChains(t, map[string]signing{"cat-apk": {key.cert}})

	// A developer app event with no proof behind it: nobody is the active developer.
	stale := &nostr.Event{
		ID: strings.Repeat("d", 63) + "1", PubKey: ownerKey, Kind: events.KindApp, CreatedAt: 3,
		Tags: nostr.Tags{{"d", testApp}, {"name", "Stale"}, {"f", "android-arm64-v8a"}},
	}
	saveSlotEvent(t, r.store, *stale)
	catApp, catRelease, catAsset := testTriple("c", catalogKey, blobs.put("cat-apk"), key.hash, 1)
	for _, event := range []*nostr.Event{catApp, catAsset, catRelease} {
		r.holdListing(t.Context(), event)
	}
	drainReads(t, r)
	if !slotHas(t, r.store, catApp.ID) || !slotHas(t, r.store, catRelease.ID) {
		t.Fatal("the catalog listing was not stored")
	}
	if slotHas(t, r.store, stale.ID) {
		t.Fatal("the stale app event stays next to the catalog's")
	}
}
