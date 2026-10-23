package relay

import (
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestNotAnchoredAppCoordinate(t *testing.T) {
	db := newSlotDB(t)
	catalog := testPubkey(t)
	owner := testPubkey(t)
	delegate := testPubkey(t)
	stranger := testPubkey(t)
	appID := "com.example.app"
	cert := strings.Repeat("ab", 32)
	saveSlotEvent(t, db, nostr.Event{
		ID:        strings.Repeat("1", 64),
		PubKey:    owner,
		Kind:      events.KindIdentityProof,
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		Tags:      nostr.Tags{{"d", cert}, {"expiry", "9999999999"}, {"delegation", delegate}},
	})
	saveSlotEvent(t, db, nostr.Event{
		ID:        strings.Repeat("a", 64),
		PubKey:    catalog,
		Kind:      events.KindAsset,
		CreatedAt: 1,
		Tags:      nostr.Tags{{"i", appID}, {"apk_certificate_hash", cert}},
	})
	saveSlotEvent(t, db, nostr.Event{
		ID:     strings.Repeat("2", 64),
		PubKey: delegate,
		Kind:   events.KindApp,
		Tags:   nostr.Tags{{"d", appID}},
	})

	hook := NotAnchored(db, catalog)
	catalogAddr := "32267:" + catalog + ":" + appID
	if err := hook(nil, commentOn(catalogAddr)); err != nil {
		t.Fatal(err)
	}
	if err := hook(nil, zapTo(catalogAddr, owner)); err != nil {
		t.Fatal(err)
	}
	// The delegate signed the listing; older clients pay the listing signer.
	if err := hook(nil, zapTo(catalogAddr, delegate)); err != nil {
		t.Fatal(err)
	}
	if err := hook(nil, zapTo(catalogAddr, catalog)); err != nil {
		t.Fatal(err)
	}
	if err := hook(nil, zapTo(catalogAddr, events.EarlierCatalogKey)); err != nil {
		t.Fatal(err)
	}
	if err := hook(nil, zapTo(catalogAddr, stranger)); err == nil {
		t.Fatal("stranger accepted as zap recipient")
	}
	if err := hook(nil, zapOn(catalogAddr)); err == nil {
		t.Fatal("zap with no recipient accepted")
	}
	for _, signer := range []string{owner, delegate, stranger, events.EarlierCatalogKey} {
		addr := "32267:" + signer + ":" + appID
		if err := hook(nil, commentOn(addr)); err != nil {
			t.Errorf("comment on %s: %v", addr, err)
		}
		if err := hook(nil, zapTo(addr, owner)); err != nil {
			t.Errorf("zap on %s: %v", addr, err)
		}
		if err := hook(nil, reportOn(addr)); err != nil {
			t.Errorf("report on %s: %v", addr, err)
		}
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

func TestNotAnchoredReplacedProof(t *testing.T) {
	db := newSlotDB(t)
	catalog := testPubkey(t)
	retired := testPubkey(t)
	owner := testPubkey(t)
	appID := "com.example.app"
	cert := strings.Repeat("cd", 32)
	saveSlotEvent(t, db, nostr.Event{
		ID:        strings.Repeat("1", 64),
		PubKey:    retired,
		Kind:      events.KindIdentityProof,
		CreatedAt: 10,
		Tags:      nostr.Tags{{"d", cert}, {"expiry", "9999999999"}},
	})
	saveSlotEvent(t, db, nostr.Event{
		ID:        strings.Repeat("2", 64),
		PubKey:    retired,
		Kind:      events.KindIdentityProof,
		CreatedAt: 40,
		Tags:      nostr.Tags{{"d", cert}, {"revoked"}},
	})
	saveSlotEvent(t, db, nostr.Event{
		ID:        strings.Repeat("3", 64),
		PubKey:    owner,
		Kind:      events.KindIdentityProof,
		CreatedAt: 30,
		Tags:      nostr.Tags{{"d", cert}, {"expiry", "9999999999"}},
	})
	saveSlotEvent(t, db, nostr.Event{
		ID:        strings.Repeat("a", 64),
		PubKey:    catalog,
		Kind:      events.KindAsset,
		CreatedAt: 1,
		Tags:      nostr.Tags{{"i", appID}, {"apk_certificate_hash", cert}},
	})
	saveSlotEvent(t, db, nostr.Event{
		ID:     strings.Repeat("4", 64),
		PubKey: catalog,
		Kind:   events.KindApp,
		Tags:   nostr.Tags{{"d", appID}},
	})

	hook := NotAnchored(db, catalog)
	catalogAddr := "32267:" + catalog + ":" + appID
	if err := hook(nil, zapTo(catalogAddr, owner)); err != nil {
		t.Fatal(err)
	}
	if err := hook(nil, zapTo(catalogAddr, retired)); err == nil {
		t.Fatal("retired proof accepted as zap recipient")
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
	return addressed(events.KindComment, "A", address)
}

func zapOn(address string) *nostr.Event {
	return addressed(events.KindZap, "a", address)
}

func zapTo(address, recipient string) *nostr.Event {
	event := zapOn(address)
	event.Tags = append(event.Tags, nostr.Tag{"p", recipient})
	return event
}

func TestZapWithoutProofPaysTheCatalog(t *testing.T) {
	db := newSlotDB(t)
	catalog := testPubkey(t)
	appID := "com.example.app"
	saveSlotEvent(t, db, nostr.Event{ID: strings.Repeat("1", 64), PubKey: catalog, Kind: events.KindApp, Tags: nostr.Tags{{"d", appID}}})
	hook := NotAnchored(db, catalog)
	addr := "32267:" + catalog + ":" + appID
	if err := hook(nil, zapTo(addr, catalog)); err != nil {
		t.Fatal(err)
	}
	if err := hook(nil, zapTo(addr, testPubkey(t))); err == nil {
		t.Fatal("zap accepted for an unknown pubkey")
	}
}

func TestNoZapTag(t *testing.T) {
	if err := NoZapTag(nil, &nostr.Event{Kind: events.KindApp, Tags: nostr.Tags{{"d", "com.example.app"}, {"zap", testPubkey(t)}}}); err == nil {
		t.Fatal("zap tag accepted")
	}
	if err := NoZapTag(nil, &nostr.Event{Kind: events.KindApp, Tags: nostr.Tags{{"zap"}}}); err == nil {
		t.Fatal("empty zap tag accepted")
	}
	if err := NoZapTag(nil, &nostr.Event{Kind: events.KindApp, Tags: nostr.Tags{{"d", "com.example.app"}}}); err != nil {
		t.Fatal(err)
	}
	if err := NoZapTag(nil, &nostr.Event{Kind: events.KindRelease, Tags: nostr.Tags{{"zap", "aa"}}}); err != nil {
		t.Fatal(err)
	}
}

func reportOn(address string) *nostr.Event {
	return addressed(events.KindReport, "a", address)
}

// addressed carries the i tag an app coordinate needs.
func addressed(kind int, tag, address string) *nostr.Event {
	tags := nostr.Tags{{tag, address}}
	if parts := strings.SplitN(address, ":", 3); len(parts) == 3 && parts[0] == "32267" {
		tags = append(tags, nostr.Tag{"i", parts[2]})
	}
	return &nostr.Event{Kind: kind, Tags: tags}
}

func TestReportNeedsAnAcceptedAppCoordinate(t *testing.T) {
	db := newSlotDB(t)
	catalog := testPubkey(t)
	appID := "com.example.app"
	saveSlotEvent(t, db, nostr.Event{ID: strings.Repeat("1", 64), PubKey: catalog, Kind: events.KindApp, Tags: nostr.Tags{{"d", appID}}})
	saveSlotEvent(t, db, nostr.Event{ID: strings.Repeat("2", 64), PubKey: catalog, Kind: events.KindStack, Tags: nostr.Tags{{"d", "editors"}}})
	hook := NotAnchored(db, catalog)

	if err := hook(nil, reportOn("32267:"+catalog+":"+appID)); err != nil {
		t.Fatal(err)
	}
	if err := hook(nil, reportOn("32267:"+testPubkey(t)+":"+appID)); err != nil {
		t.Fatal(err)
	}
	for name, event := range map[string]*nostr.Event{
		"no address":  {Kind: events.KindReport, Tags: nostr.Tags{{"p", catalog}}},
		"event only":  {Kind: events.KindReport, Tags: nostr.Tags{{"e", strings.Repeat("1", 64)}}},
		"stack":       reportOn("30267:" + catalog + ":editors"),
		"missing app": reportOn("32267:" + catalog + ":com.other.app"),
	} {
		if err := hook(nil, event); err == nil {
			t.Errorf("%s: report accepted", name)
		}
	}
}

func TestAppCoordinateNeedsTheAppIDTag(t *testing.T) {
	db := newSlotDB(t)
	catalog := testPubkey(t)
	appID := "com.example.app"
	saveSlotEvent(t, db, nostr.Event{ID: strings.Repeat("1", 64), PubKey: catalog, Kind: events.KindApp, Tags: nostr.Tags{{"d", appID}}})
	hook := NotAnchored(db, catalog)
	addr := "32267:" + catalog + ":" + appID
	for _, kind := range []int{events.KindComment, events.KindReport} {
		tag := "a"
		if kind == events.KindComment {
			tag = "A"
		}
		for _, tags := range []nostr.Tags{
			{{tag, addr}},
			{{tag, addr}, {"i", "com.other.app"}},
		} {
			if err := hook(nil, &nostr.Event{Kind: kind, Tags: tags}); err == nil {
				t.Errorf("kind %d with tags %v accepted", kind, tags)
			}
		}
	}
	for _, tags := range []nostr.Tags{
		{{"a", addr}, {"p", catalog}},
		{{"a", addr}, {"p", catalog}, {"i", "com.other.app"}},
	} {
		if err := hook(nil, &nostr.Event{Kind: events.KindZap, Tags: tags}); err != nil {
			t.Errorf("zap with tags %v: %v", tags, err)
		}
	}
}
