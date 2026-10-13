package catalog

import (
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestSelectListingKeepsVariantAsset(t *testing.T) {
	const (
		appID   = "com.example.app"
		pubkey  = "aa9047325603dacd4f8142093567973566de3b1e20a89557b728c3be4c6a844b"
		assetID = "fe6469842079b0fc4f5ea0d30518c720f239324235a81a1359e2969dd2ac0546"
	)
	hash := strings.Repeat("ab", 32)
	cert := strings.Repeat("cd", 32)
	app := nostr.Event{Kind: events.KindApp, PubKey: pubkey, Tags: nostr.Tags{{"d", appID}}}
	release := nostr.Event{Kind: events.KindRelease, PubKey: pubkey, Tags: nostr.Tags{
		{"i", appID},
		{"version", "1.17.0"},
		{"d", appID + "@1.17.0"},
		{"c", "main"},
		{"e", assetID},
	}}
	asset := nostr.Event{ID: assetID, Kind: events.KindAsset, PubKey: pubkey, Tags: nostr.Tags{
		{"i", appID},
		{"x", hash},
		{"version", "1.17.0"},
		{"m", "application/vnd.android.package-archive"},
		{"f", "android-arm64-v8a"},
		{"version_code", "462"},
		{"apk_certificate_hash", cert},
		{"variant", "google"},
	}}
	listing, ok := selectListing(appID, app, []nostr.Event{release}, map[string]nostr.Event{assetID: asset})
	if !ok {
		t.Fatal("variant asset was dropped")
	}
	if len(listing.Assets) != 1 || listing.Assets[0].ID != assetID {
		t.Fatalf("assets %v", listing.Assets)
	}
}
