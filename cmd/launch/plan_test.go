package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/catalog"
	"github.com/zapstore/relay/pkg/events"
)

const testNewKey = "aa9047325603dacd4f8142093567973566de3b1e20a89557b728c3be4c6a844b"

// oldListing is a complete 78ce… listing for appID.
func oldListing(appID string, n int) []nostr.Event {
	assetID := strings.Repeat(string(rune('a'+n)), 64)
	cert := strings.Repeat("cd", 32)
	return []nostr.Event{
		{ID: "app-" + appID, PubKey: oldKey, Kind: events.KindApp, CreatedAt: 100, Tags: nostr.Tags{
			{"d", appID}, {"name", appID}, {"f", "android-arm64-v8a"}, {"zap", oldKey, "wss://relay.zapstore.dev", "1"},
		}},
		{ID: "release-" + appID, PubKey: oldKey, Kind: events.KindRelease, CreatedAt: 100, Tags: nostr.Tags{
			{"i", appID}, {"version", "1.0"}, {"d", appID + "@1.0"}, {"c", "main"},
			{"a", appCoord(oldKey, appID)}, {"e", assetID},
		}},
		{ID: assetID, PubKey: oldKey, Kind: events.KindAsset, CreatedAt: 100, Tags: nostr.Tags{
			{"i", appID}, {"x", strings.Repeat("ab", 32)}, {"version", "1.0"},
			{"m", "application/vnd.android.package-archive"}, {"f", "android-arm64-v8a"},
			{"version_code", "1"}, {"apk_certificate_hash", cert}, {"apk_certificate_hash", strings.Repeat("ef", 32)},
		}},
	}
}

func noLineage(string, string) ([]string, error) { return nil, nil }

func TestBuildPlan(t *testing.T) {
	var old []nostr.Event
	old = append(old, oldListing("com.example.keep", 0)...)
	old = append(old, oldListing("com.example.gone", 1)...)
	old = append(old, oldListing("dev.zapstore.app", 2)...)
	old = append(old,
		nostr.Event{ID: "del", PubKey: oldKey, Kind: events.KindDeletion, CreatedAt: 200, Tags: nostr.Tags{{"a", appCoord(oldKey, "com.example.gone")}}},
		nostr.Event{ID: "public-stack", PubKey: oldKey, Kind: events.KindStack, Tags: nostr.Tags{{"d", "s"}}},
		nostr.Event{ID: "private-stack", PubKey: oldKey, Kind: events.KindStack, Content: "nip44", Tags: nostr.Tags{{"d", "p"}}},
		nostr.Event{ID: "relays", PubKey: oldKey, Kind: events.KindAppRelays},
		nostr.Event{ID: "profile", PubKey: oldKey, Kind: events.KindProfile, Content: `{"name":"Zapstore"}`},
	)
	p, err := buildPlan(old, noLineage, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.appIDs(); !slices.Equal(got, []string{"com.example.keep"}) {
		t.Errorf("re-signed apps = %v, want [com.example.keep]", got)
	}
	if !slices.Equal(p.Kept, []string{"dev.zapstore.app"}) {
		t.Errorf("kept = %v", p.Kept)
	}
	if !slices.Equal(p.Dropped, []string{"com.example.gone"}) {
		t.Errorf("dropped = %v", p.Dropped)
	}
	if len(p.Stacks) != 1 || p.Stacks[0].ID != "public-stack" {
		t.Errorf("stacks = %v", p.Stacks)
	}
	for _, id := range []string{"app-dev.zapstore.app", "release-dev.zapstore.app", "profile"} {
		if slices.Contains(p.Delete, id) {
			t.Errorf("plan deletes %s", id)
		}
	}
	for _, id := range []string{"app-com.example.gone", "del", "private-stack", "public-stack", "relays"} {
		if !slices.Contains(p.Delete, id) {
			t.Errorf("plan keeps %s", id)
		}
	}
}

func TestBuildPlanMissingAsset(t *testing.T) {
	old := oldListing("com.example.app", 0)
	old[1].Tags = append(old[1].Tags, nostr.Tag{"e", strings.Repeat("f", 64)})
	if _, err := buildPlan(old, noLineage, time.Unix(1000, 0)); err == nil {
		t.Fatal("a release naming an unknown asset was planned")
	}
}

func TestResign(t *testing.T) {
	p, err := buildPlan(oldListing("com.example.app", 0), func(string, string) ([]string, error) {
		return []string{"old1", "old2"}, nil
	}, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	sk := nostr.GeneratePrivateKey()
	signer, err := catalog.ParseSigner(sk)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := resign(t.Context(), p, signer.Sign, signer.PubKey, "zap@example.com", time.Unix(2000, 0))
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]int, len(evs))
	for i, e := range evs {
		kinds[i] = e.Kind
		if ok, _ := e.CheckSignature(); !ok {
			t.Errorf("kind %d signature does not verify", e.Kind)
		}
	}
	want := []int{events.KindAsset, events.KindRelease, events.KindApp, events.KindProfile, events.KindAppRelays}
	if !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	asset, release, app := evs[0], evs[1], evs[2]
	if got := events.FindAll(asset.Tags, "apk_certificate_hash"); len(got) != 1 {
		t.Errorf("asset certificates = %v, want one", got)
	}
	if got := events.FindAll(asset.Tags, "apk_certificate_ancestor"); !slices.Equal(got, []string{"old1", "old2"}) {
		t.Errorf("asset ancestors = %v", got)
	}
	if asset.CreatedAt != 100 {
		t.Errorf("asset created_at = %d, want 100", asset.CreatedAt)
	}
	if e, _ := events.Find(release.Tags, "e"); e != asset.ID {
		t.Errorf("release e = %s, want %s", e, asset.ID)
	}
	if a, _ := events.Find(release.Tags, "a"); a != appCoord(signer.PubKey, "com.example.app") {
		t.Errorf("release a = %s", a)
	}
	if f := events.FindAll(release.Tags, "f"); !slices.Equal(f, []string{platformARM64}) {
		t.Errorf("release f = %v, want [%s]", f, platformARM64)
	}
	if _, ok := events.Find(app.Tags, "zap"); ok {
		t.Error("app kept its zap tag")
	}
	if lud16Of(&evs[3]) != "zap@example.com" {
		t.Errorf("kind 0 content = %s", evs[3].Content)
	}
}

func TestStackTags(t *testing.T) {
	got := stackTags(nostr.Tags{
		{"d", "favorites"},
		{"a", appCoord(oldKey, "com.example.app")},
		{"a", appCoord(oldKey, "dev.zapstore.app")},
		{"a", appCoord(testNewKey, "org.other")},
	}, testNewKey)
	want := []string{appCoord(testNewKey, "com.example.app"), appCoord(oldKey, "dev.zapstore.app"), appCoord(testNewKey, "org.other")}
	if a := events.FindAll(got, "a"); !slices.Equal(a, want) {
		t.Errorf("a tags = %v, want %v", a, want)
	}
	if f := events.FindAll(got, "f"); !slices.Equal(f, []string{platformARM64}) {
		t.Errorf("f tags = %v, want [%s]", f, platformARM64)
	}
}

func TestReleaseTagsAddsA(t *testing.T) {
	coord := appCoord(testNewKey, "com.example.app")
	got, err := releaseTags(nostr.Tags{{"i", "com.example.app"}, {"e", "old"}}, coord, map[string]string{"old": "new"})
	if err != nil {
		t.Fatal(err)
	}
	if a := events.FindAll(got, "a"); !slices.Equal(a, []string{coord}) {
		t.Errorf("a tags = %v, want [%s]", a, coord)
	}
	if e := events.FindAll(got, "e"); !slices.Equal(e, []string{"new"}) {
		t.Errorf("e tags = %v, want [new]", e)
	}
}

func TestAssetTagsKeepsAncestorsWithoutLineage(t *testing.T) {
	got := assetTags(nostr.Tags{{"apk_certificate_hash", "c"}, {"apk_certificate_ancestor", "a"}}, nil)
	if a := events.FindAll(got, "apk_certificate_ancestor"); !slices.Equal(a, []string{"a"}) {
		t.Errorf("ancestors = %v, want [a]", a)
	}
}

func TestProfileContent(t *testing.T) {
	got, err := profileContent(`{"name":"Zapstore","lud16":"old@example.com"}`, "new@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"lud16":"new@example.com","name":"Zapstore"}` {
		t.Errorf("content = %s", got)
	}
}

func TestIntervalOn(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want bool
	}{{"", true}, {"0", false}, {"0s", false}, {"5m", true}} {
		got, err := intervalOn(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("intervalOn(%q) = %v, %v, want %v", tt.in, got, err, tt.want)
		}
	}
	if _, err := intervalOn("soon"); err == nil {
		t.Error("intervalOn(soon) accepted")
	}
}
