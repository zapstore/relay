package relay

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/relay/store"
)

func TestListingCertificate(t *testing.T) {
	db := newSlotDB(t)
	catalog := strings.Repeat("c", 64)
	owner := strings.Repeat("a", 64)
	cert := strings.Repeat("e", 64)
	successor := strings.Repeat("f", 64)
	appID := "com.example.app"
	hook := ListingCertificate(db, catalog)

	if err := hook(nil, slotAsset(owner, appID, cert)); err != nil {
		t.Fatalf("an unpinned app waits for promotion: %v", err)
	}
	if err := hook(nil, &nostr.Event{Kind: events.KindApp, PubKey: owner, Tags: nostr.Tags{{"d", appID}}}); err != nil {
		t.Fatalf("an app event waits for promotion: %v", err)
	}

	pinned := slotAsset(catalog, appID, cert)
	pinned.ID = strings.Repeat("1", 64)
	pinned.CreatedAt = 1
	saveSlotEvent(t, db, *pinned)
	if err := hook(nil, slotAsset(owner, appID, cert)); err != nil {
		t.Fatal(err)
	}
	rotated := slotAsset(owner, appID, successor)
	rotated.Tags = append(rotated.Tags, nostr.Tag{"apk_certificate_ancestor", cert})
	if err := hook(nil, rotated); err != nil {
		t.Fatalf("a lineage naming the pin: %v", err)
	}
	if err := hook(nil, slotAsset(owner, appID, successor)); !errors.Is(err, errCertificate) {
		t.Fatalf("wrong cert: %v", err)
	}
	if err := hook(nil, slotAsset(catalog, appID, successor)); err != nil {
		t.Fatalf("the indexer moves the pin at promotion: %v", err)
	}
}

func TestSupersedeKeepsTheCurrentListingWholeAndOlderReleases(t *testing.T) {
	db := newSlotDB(t)
	ctx := context.Background()
	catalog := strings.Repeat("c", 64)
	owner := strings.Repeat("a", 64)
	delegate := strings.Repeat("b", 64)
	former := strings.Repeat("d", 64)
	cert := strings.Repeat("e", 64)
	appID := "com.example.app"
	id := func(n string) string { return strings.Repeat(n, 64) }
	asset := func(n, pub, code string) {
		saveSlotEvent(t, db, nostr.Event{
			ID: id(n), PubKey: pub, Kind: events.KindAsset,
			Tags: nostr.Tags{{"i", appID}, {"apk_certificate_hash", cert}, {"version_code", code}},
		})
	}
	release := func(n, pub, assetID, version string) nostr.Event {
		event := nostr.Event{
			ID: id(n), PubKey: pub, Kind: events.KindRelease,
			Tags: nostr.Tags{{"i", appID}, {"d", appID + "@" + version}, {"c", "main"}, {"e", id(assetID)}},
		}
		saveSlotEvent(t, db, event)
		return event
	}
	saveSlotEvent(t, db, nostr.Event{
		ID:        id("1"),
		PubKey:    owner,
		Kind:      events.KindIdentityProof,
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		Tags:      nostr.Tags{{"d", cert}, {"expiry", "9999999999"}, {"delegation", delegate}},
	})
	saveSlotEvent(t, db, nostr.Event{ID: id("2"), PubKey: catalog, Kind: events.KindApp, Tags: nostr.Tags{{"d", appID}}})
	asset("a", catalog, "3")
	release("b", catalog, "a", "1.3")
	saveSlotEvent(t, db, nostr.Event{ID: id("4"), PubKey: former, Kind: events.KindApp, Tags: nostr.Tags{{"d", appID}}})
	asset("c", former, "4")
	release("d", former, "c", "1.4")
	asset("e", former, "5")
	release("f", former, "e", "1.5")
	saveSlotEvent(t, db, nostr.Event{ID: id("7"), PubKey: delegate, Kind: events.KindApp, Tags: nostr.Tags{{"d", appID}}})
	asset("6", delegate, "5")
	current := release("8", delegate, "6", "1.5")

	r := &T{store: db, config: Config{Info: Info{Pubkey: catalog}}}
	if err := r.supersede(ctx, &current, delegate); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"the current app event":           slotHas(t, db, id("7")),
		"the catalog key's older release": slotHas(t, db, id("b")) && slotHas(t, db, id("a")),
		"a former owner's older release":  slotHas(t, db, id("d")) && slotHas(t, db, id("c")),
	} {
		if !want {
			t.Errorf("%s was deleted", name)
		}
	}
	for name, id := range map[string]string{
		"the catalog key's app event":               id("2"),
		"a former owner's app event":                id("4"),
		"a former owner's release at the same code": id("f"),
		"its asset": id("e"),
	} {
		if slotHas(t, db, id) {
			t.Errorf("%s remains", name)
		}
	}

	// A release that is not the current one changes nothing.
	saveSlotEvent(t, db, nostr.Event{ID: id("9"), PubKey: catalog, Kind: events.KindApp, Tags: nostr.Tags{{"d", appID}}})
	older := release("b", catalog, "a", "1.3")
	if err := r.supersede(ctx, &older, catalog); err != nil {
		t.Fatal(err)
	}
	if !slotHas(t, db, id("7")) {
		t.Fatal("an older release deleted the current app event")
	}

	kind5 := &nostr.Event{
		ID: strings.Repeat("a", 63) + "1", PubKey: delegate, Kind: nostr.KindDeletion,
		Tags: nostr.Tags{{"a", "32267:" + delegate + ":" + appID}},
	}
	if err := r.handleDelete(ctx, kind5); err != nil {
		t.Fatal(err)
	}
	if slotHas(t, db, id("7")) || slotHas(t, db, id("8")) {
		t.Fatal("listings remain after developer removal")
	}
	if !slotHas(t, db, kind5.ID) {
		t.Fatal("kind 5 was not stored")
	}
}

func newSlotDB(t *testing.T) store.T {
	t.Helper()
	db, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func saveSlotEvent(t *testing.T, db store.T, event nostr.Event) {
	t.Helper()
	if event.CreatedAt == 0 {
		event.CreatedAt = 1
	}
	if _, err := db.Save(context.Background(), &event); err != nil {
		t.Fatal(err)
	}
}

func slotAsset(pub, appID, cert string) *nostr.Event {
	return &nostr.Event{
		Kind:   events.KindAsset,
		PubKey: pub,
		Tags: nostr.Tags{
			{"i", appID},
			{"x", strings.Repeat("f", 64)},
			{"version", "1.0.0"},
			{"f", "android-arm64-v8a"},
			{"version_code", "1"},
			{"apk_certificate_hash", cert},
		},
	}
}

func slotHas(t *testing.T, db store.T, id string) bool {
	t.Helper()
	var n int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM events WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}
