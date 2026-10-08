package relay

import (
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestNotAnchoredCatalogCoordinate(t *testing.T) {
	db := newSlotDB(t)
	catalog := testPubkey(t)
	developer := testPubkey(t)
	stranger := testPubkey(t)
	appID := "com.example.app"
	saveSlotEvent(t, db, nostr.Event{
		ID:     strings.Repeat("2", 64),
		PubKey: developer,
		Kind:   events.KindApp,
		Tags:   nostr.Tags{{"d", appID}},
	})

	hook := NotAnchored(db, catalog)
	catalogAddr := "32267:" + catalog + ":" + appID
	if err := hook(nil, commentOn(catalogAddr)); err != nil {
		t.Fatal(err)
	}
	if err := hook(nil, zapOn(catalogAddr)); err != nil {
		t.Fatal(err)
	}
	if err := hook(nil, commentOn("32267:"+developer+":"+appID)); err != nil {
		t.Fatal(err)
	}
	if err := hook(nil, commentOn("32267:"+stranger+":"+appID)); err == nil {
		t.Fatal("stranger coordinate accepted")
	}
	if err := hook(nil, commentOn("32267:"+catalog+":com.other.app")); err == nil {
		t.Fatal("missing app accepted")
	}

	saveSlotEvent(t, db, nostr.Event{
		ID:     strings.Repeat("3", 64),
		PubKey: catalog,
		Kind:   events.KindStack,
		Tags:   nostr.Tags{{"d", "editors"}},
	})
	if err := hook(nil, commentOn("30267:"+catalog+":editors")); err != nil {
		t.Fatal(err)
	}
	if err := hook(nil, commentOn("30267:"+catalog+":missing")); err == nil {
		t.Fatal("missing stack accepted")
	}
}

func TestNotAnchoredRequiresAListing(t *testing.T) {
	db := newSlotDB(t)
	catalog := testPubkey(t)
	hook := NotAnchored(db, catalog)
	if err := hook(nil, commentOn("32267:"+catalog+":com.example.app")); err == nil {
		t.Fatal("catalog coordinate accepted with no listing")
	}
}

func testPubkey(t *testing.T) string {
	t.Helper()
	secret := nostr.GeneratePrivateKey()
	public, err := nostr.GetPublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	return public
}

func commentOn(address string) *nostr.Event {
	return &nostr.Event{Kind: events.KindComment, Tags: nostr.Tags{{"A", address}}}
}

func zapOn(address string) *nostr.Event {
	return &nostr.Event{Kind: events.KindZap, Tags: nostr.Tags{{"a", address}}}
}
