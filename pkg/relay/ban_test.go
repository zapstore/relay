package relay

import (
	"errors"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/policy"
)

func banRelay(t *testing.T) (*T, *policy.Store) {
	t.Helper()
	r := testRelay(t, newFakeBlossom())
	pol, err := policy.Open(r.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	r.pol = pol
	return r, pol
}

func TestBanRefusesTheCatalogKey(t *testing.T) {
	r, pol := banRelay(t)
	saveListing(t, r.store, catalogKey, testApp, strings.Repeat("cd", 32), "1", "1")
	_, err := ApplyBan(t.Context(), r.store, pol, catalogKey, policy.Publisher{Kind: policy.KindNostr, ID: catalogKey}, "oops")
	if !errors.Is(err, errBanCatalogKey) {
		t.Fatalf("err = %v", err)
	}
	if banned, _ := pol.Blacklisted(t.Context(), policy.Publisher{Kind: policy.KindNostr, ID: catalogKey}); banned {
		t.Fatal("the catalog key was blacklisted")
	}
	if !slotHas(t, r.store, strings.Repeat("10", 32)) {
		t.Fatal("the catalog's listing was deleted")
	}
}

func TestDeveloperBanClearsBothSlots(t *testing.T) {
	r, pol := banRelay(t)
	cert := strings.Repeat("cd", 32)
	developer := strings.Repeat("d", 64)
	saveListing(t, r.store, catalogKey, testApp, cert, "1", "1")
	saveListing(t, r.store, developer, testApp, cert, "2", "2")
	saveListing(t, r.store, catalogKey, "com.example.other", cert, "3", "1")

	if _, err := ApplyBan(t.Context(), r.store, pol, catalogKey, policy.Publisher{Kind: policy.KindNostr, ID: developer}, "malware"); err != nil {
		t.Fatal(err)
	}
	for _, seed := range []string{"1", "2"} {
		if slotHas(t, r.store, strings.Repeat(seed+"0", 32)) {
			t.Fatalf("listing %s survived the ban", seed)
		}
	}
	if !slotHas(t, r.store, strings.Repeat("30", 32)) {
		t.Fatal("an unrelated app was deleted")
	}
	notices, err := pol.Notices(t.Context())
	if err != nil || notices[testApp] != "malware" {
		t.Fatalf("notices %v err %v", notices, err)
	}
}

func TestBanDropsBufferedEvents(t *testing.T) {
	r, pol := banRelay(t)
	developer := strings.Repeat("d", 64)
	// The blob was never uploaded, so the triple waits in the buffer zone.
	app, release, asset := testTriple("d", developer, strings.Repeat("ab", 32), strings.Repeat("cd", 32), 1)
	for _, event := range []*nostr.Event{app, release, asset} {
		if _, err := r.store.SavePending(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ApplyBan(t.Context(), r.store, pol, catalogKey, policy.Publisher{Kind: policy.KindNostr, ID: developer}, "spam"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{app.ID, release.ID, asset.ID} {
		if kept, err := pendingHas(t.Context(), r.store, id); err != nil || kept {
			t.Fatalf("pending %s: kept %v err %v", id, kept, err)
		}
	}
}

func TestBanRecordedWhileBufferedRefusesPromotion(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	pol, err := policy.Open(r.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	r.pol = pol
	x := blobs.put("apk")
	app, release, asset := testTriple("f", catalogKey, x, strings.Repeat("cd", 32), 1)
	app.Tags = append(app.Tags, nostr.Tag{"repository", "https://github.com/evil/app"})
	if res := r.holdListing(t.Context(), app); !res.ok {
		t.Fatalf("app: %+v", res)
	}
	if res := r.holdListing(t.Context(), release); !res.ok {
		t.Fatalf("release: %+v", res)
	}
	if err := pol.RecordBan(t.Context(), policy.Publisher{Kind: policy.KindForge, ID: "github.com/evil"}, "malware", nil, nil); err != nil {
		t.Fatal(err)
	}
	res := r.holdListing(t.Context(), asset)
	if res.ok || res.msg != ErrPublisherNotAllowed.Error() {
		t.Fatalf("asset: %+v", res)
	}
	for _, id := range []string{app.ID, release.ID, asset.ID} {
		if slotHas(t, r.store, id) {
			t.Fatalf("promoted %s after the ban", id)
		}
	}
}
