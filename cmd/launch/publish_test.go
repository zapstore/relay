package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/policy"
	"github.com/zapstore/relay/pkg/relay/store"
)

func TestStoreAllSkipsABannedListing(t *testing.T) {
	data := t.TempDir()
	sk := nostr.GeneratePrivateKey()
	banned := strings.Repeat("b", 64)
	var evs []nostr.Event
	for _, l := range []struct{ app, cert string }{{"com.ok", strings.Repeat("a", 64)}, {"com.bad", banned}} {
		for _, e := range []nostr.Event{
			{Kind: events.KindAsset, Tags: nostr.Tags{{"i", l.app}, {"x", strings.Repeat("c", 64)}, {"apk_certificate_hash", l.cert}}},
			{Kind: events.KindRelease, Tags: nostr.Tags{{"i", l.app}, {"d", l.app + "@1"}}},
			{Kind: events.KindApp, Tags: nostr.Tags{{"d", l.app}}},
		} {
			if err := e.Sign(sk); err != nil {
				t.Fatal(err)
			}
			evs = append(evs, e)
		}
	}
	db, err := store.New(filepath.Join(data, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	pol, err := policy.Open(db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if err := pol.RecordBan(t.Context(), policy.Publisher{Kind: policy.KindCert, ID: banned}, "malware", nil, nil); err != nil {
		t.Fatal(err)
	}
	db.Close()

	rep, err := storeAll(t.Context(), newTerm(strings.NewReader(""), io.Discard), data, evs)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stored != 3 || !rep.Banned["com.bad"] || rep.Banned["com.ok"] {
		t.Fatalf("report %+v", rep)
	}
	db, err = store.New(filepath.Join(data, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, e := range evs {
		got, err := db.Has(t.Context(), nostr.Filter{IDs: []string{e.ID}})
		if err != nil {
			t.Fatal(err)
		}
		if want := eventAppID(e) == "com.ok"; got != want {
			t.Fatalf("%s kind %d stored %v", eventAppID(e), e.Kind, got)
		}
	}
}

func TestSkipBannedListing(t *testing.T) {
	asset := nostr.Event{Kind: events.KindAsset, Tags: nostr.Tags{{"i", "com.example"}}}
	release := nostr.Event{Kind: events.KindRelease, Tags: nostr.Tags{{"i", "com.example"}}}
	app := nostr.Event{Kind: events.KindApp, Tags: nostr.Tags{{"d", "com.example"}}}
	stack := nostr.Event{Kind: events.KindStack, Tags: nostr.Tags{{"d", "com.example"}}}
	banned := map[string]bool{}
	if skipEvent(asset, banned) {
		t.Fatal("an asset was skipped without a ban")
	}
	banned[eventAppID(asset)] = true
	if !skipEvent(asset, banned) || !skipEvent(release, banned) || !skipEvent(app, banned) {
		t.Fatal("part of a banned listing was stored")
	}
	if skipEvent(stack, banned) {
		t.Fatal("a stack was skipped with the listing")
	}
}
