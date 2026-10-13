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

func TestListingSlots(t *testing.T) {
	db := newSlotDB(t)
	catalog := strings.Repeat("c", 64)
	owner := strings.Repeat("a", 64)
	cert := strings.Repeat("e", 64)
	successor := strings.Repeat("f", 64)
	appID := "com.example.app"
	hook := ListingSlots(db, catalog)

	if err := hook(nil, slotAsset(owner, appID, cert)); err != nil {
		t.Fatalf("an unpinned app waits for promotion: %v", err)
	}
	if err := hook(nil, &nostr.Event{Kind: events.KindApp, PubKey: owner, Tags: nostr.Tags{{"d", appID}}}); err != nil {
		t.Fatalf("an app event waits for promotion: %v", err)
	}

	if err := setAppCertificate(context.Background(), db, appID, cert, false); err != nil {
		t.Fatal(err)
	}
	if err := hook(nil, slotAsset(owner, appID, cert)); err != nil {
		t.Fatal(err)
	}
	rotated := slotAsset(owner, appID, successor)
	rotated.Tags = append(rotated.Tags, nostr.Tag{"apk_certificate_hash", cert})
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

func TestDeveloperSlotReplacementAndRemoval(t *testing.T) {
	db := newSlotDB(t)
	ctx := context.Background()
	catalog := strings.Repeat("c", 64)
	owner := strings.Repeat("a", 64)
	delegate := strings.Repeat("b", 64)
	cert := strings.Repeat("e", 64)
	appID := "com.example.app"
	saveSlotEvent(t, db, nostr.Event{
		ID:        strings.Repeat("1", 64),
		PubKey:    owner,
		Kind:      events.KindIdentityProof,
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		Tags:      nostr.Tags{{"d", cert}, {"expiry", "9999999999"}, {"delegation", delegate}},
	})
	if err := setAppCertificate(ctx, db, appID, cert, false); err != nil {
		t.Fatal(err)
	}
	saveSlotEvent(t, db, nostr.Event{ID: strings.Repeat("2", 64), PubKey: catalog, Kind: events.KindApp, Tags: nostr.Tags{{"d", appID}}})
	oldAsset := strings.Repeat("3", 64)
	saveSlotEvent(t, db, nostr.Event{ID: oldAsset, PubKey: owner, Kind: events.KindAsset, Tags: nostr.Tags{{"i", appID}}})
	saveSlotEvent(t, db, nostr.Event{ID: strings.Repeat("4", 64), PubKey: owner, Kind: events.KindApp, Tags: nostr.Tags{{"d", appID}}})
	saveSlotEvent(t, db, nostr.Event{
		ID: strings.Repeat("5", 64), PubKey: owner, Kind: events.KindRelease,
		Tags: nostr.Tags{{"i", appID}, {"e", oldAsset}},
	})
	newAsset := strings.Repeat("6", 64)
	saveSlotEvent(t, db, nostr.Event{ID: newAsset, PubKey: delegate, Kind: events.KindAsset, Tags: nostr.Tags{{"i", appID}}})
	saveSlotEvent(t, db, nostr.Event{ID: strings.Repeat("7", 64), PubKey: delegate, Kind: events.KindApp, Tags: nostr.Tags{{"d", appID}}})
	release := nostr.Event{
		ID: strings.Repeat("8", 64), PubKey: delegate, Kind: events.KindRelease,
		Tags: nostr.Tags{{"i", appID}, {"e", newAsset}},
	}
	saveSlotEvent(t, db, release)

	r := &T{store: db, config: Config{Info: Info{Pubkey: catalog}}}
	r.replaceOtherDeveloper(ctx, &release)
	if slotHas(t, db, strings.Repeat("4", 64)) {
		t.Fatal("old developer app remains")
	}
	if !slotHas(t, db, strings.Repeat("2", 64)) || !slotHas(t, db, strings.Repeat("7", 64)) {
		t.Fatal("indexer or new developer listing missing")
	}

	kind5 := &nostr.Event{
		ID: strings.Repeat("9", 64), PubKey: delegate, Kind: nostr.KindDeletion,
		Tags: nostr.Tags{{"a", "32267:" + delegate + ":" + appID}},
	}
	if err := r.handleDelete(ctx, kind5); err != nil {
		t.Fatal(err)
	}
	if slotHas(t, db, strings.Repeat("2", 64)) || slotHas(t, db, strings.Repeat("7", 64)) {
		t.Fatal("listings remain after developer removal")
	}
	var n int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM app_certificate WHERE app_id = ?`, appID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("certificate pin remains")
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
