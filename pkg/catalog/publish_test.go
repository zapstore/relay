package catalog

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/steroid/artifacts"
)

func TestPublishMatchAPKOmitsStaleArtifacts(t *testing.T) {
	data := t.TempDir()
	const current = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const previous = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	next := State{Listings: []Listing{
		apkListing("com.example.match", current),
		apkListing("com.example.stale", current),
		apkListing("com.example.oldcache", current),
		apkListing("com.example.nocache", current),
		apkListing("com.example.nohash", ""),
	}}
	for _, id := range []string{
		"com.example.match",
		"com.example.stale",
		"com.example.oldcache",
		"com.example.nocache",
		"com.example.nohash",
	} {
		writeAbout(t, data, id)
	}
	writeCache(t, data, "com.example.match", strings.ToUpper(current))
	writeCache(t, data, "com.example.stale", previous)
	writeCache(t, data, "com.example.oldcache", current)
	writeCache(t, data, "com.example.nohash", "")

	n, err := CreateBundle(t.Context(), data, next, signer, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("epoch = %d", n)
	}
	body, err := os.ReadFile(BundlePath(data, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	files := tarFiles(t, body)
	for _, id := range []string{"com.example.match", "com.example.oldcache"} {
		if _, ok := files[id+"/about"]; !ok {
			t.Fatalf("missing %s/about in %v", id, keys(files))
		}
	}
	for _, id := range []string{"com.example.stale", "com.example.nocache", "com.example.nohash"} {
		if _, ok := files[id+"/about"]; ok {
			t.Fatalf("shipped %s/about", id)
		}
	}
	if !strings.Contains(string(files["diff.jsonl"]), "com.example.stale-asset") {
		t.Fatalf("stale app events missing: %s", files["diff.jsonl"])
	}
}

func TestFromZeroMatchesSnapshot(t *testing.T) {
	data := t.TempDir()
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	pub := strings.Repeat("ab", 32)
	assetID := strings.Repeat("cd", 32)
	hash := strings.Repeat("ef", 32)
	const appID = "com.example.maps"
	eventsIn := []nostr.Event{
		{
			ID: appID + "-app", Kind: events.KindApp, PubKey: pub,
			Tags: nostr.Tags{{"d", appID}, {"name", "Maps"}, {"f", "android-arm64-v8a"}},
		},
		{
			ID: appID + "-rel", Kind: events.KindRelease, PubKey: pub,
			Tags: nostr.Tags{
				{"i", appID}, {"version", "1.0.0"}, {"d", appID + "@1.0.0"},
				{"c", "main"}, {"e", assetID},
			},
		},
		{
			ID: assetID, Kind: events.KindAsset, PubKey: pub,
			Tags: nostr.Tags{
				{"i", appID}, {"x", hash}, {"version", "1.0.0"},
				{"f", "android-arm64-v8a"}, {"version_code", "1"},
				{"apk_certificate_hash", hash},
				{"url", "https://example.com/maps.apk"},
			},
		},
	}
	state := Resolve(eventsIn, "")
	if len(state.Listings) != 1 {
		t.Fatalf("listings = %d", len(state.Listings))
	}
	n, err := CreateBundle(t.Context(), data, state, signer, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(BundlePath(data, 0, n))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(BundlePath(data, 0, n)); err != nil {
		t.Fatal(err)
	}
	body, err := EnsureBundle(t.Context(), data, 0, n, signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, body) {
		t.Fatal("a rebuilt bundle differs from the one the tick wrote")
	}
	snap, err := ReadSnapEvents(SnapPath(data, n))
	if err != nil {
		t.Fatal(err)
	}
	diff := string(tarFiles(t, body)["diff.jsonl"])
	if len(snap) == 0 {
		t.Fatal("snapshot is empty")
	}
	for _, event := range snap {
		if !strings.Contains(diff, event.ID) {
			t.Fatalf("snapshot event %s missing from diff", event.ID)
		}
	}
}

func TestRequestBuiltBundleCarriesBanLines(t *testing.T) {
	data := t.TempDir()
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	spy := resolvedListing(t, "com.example.spy")
	maps := resolvedListing(t, "com.example.maps")
	other := resolvedListing(t, "com.example.other")
	for i, st := range []State{
		{Listings: []Listing{spy, maps, other}},
		{Listings: []Listing{spy, maps}},
		{Listings: []Listing{maps}},
	} {
		if _, err := CreateBundle(t.Context(), data, st, signer, int64(10+i), nil); err != nil {
			t.Fatal(err)
		}
	}
	notices := func(context.Context) (map[string]string, error) {
		return map[string]string{"com.example.spy": "Records the microphone."}, nil
	}
	body, err := EnsureBundle(t.Context(), data, 1, 3, signer, notices)
	if err != nil {
		t.Fatal(err)
	}
	diff := string(tarFiles(t, body)["diff.jsonl"])
	if !strings.Contains(diff, `"delete":["com.example.other","com.example.spy"]`) {
		t.Fatalf("diff = %s", diff)
	}
	if !strings.Contains(diff, `{"banned":true,"app_id":"com.example.spy","reason":"Records the microphone."}`) {
		t.Fatalf("missing ban line: %s", diff)
	}
	if strings.Contains(diff, `"app_id":"com.example.other"`) {
		t.Fatalf("ban line for a normal removal: %s", diff)
	}
}

func TestBundleEnrichesAppsInTheDiff(t *testing.T) {
	data := t.TempDir()
	const current = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	next := State{Listings: []Listing{
		apkListing("com.example.b", current),
		apkListing("com.example.a", current),
	}}
	var got []string
	enrich := func(ctx context.Context, ids []string) error {
		got = append(got, ids...)
		for _, id := range ids {
			writeAbout(t, data, id)
			writeCache(t, data, id, current)
		}
		return nil
	}
	n, err := CreateBundle(t.Context(), data, next, signer, 10, enrich)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("epoch = %d", n)
	}
	if strings.Join(got, ",") != "com.example.b,com.example.a" {
		t.Fatalf("enriched %v", got)
	}
	body, err := os.ReadFile(BundlePath(data, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	files := tarFiles(t, body)
	for _, id := range []string{"com.example.a", "com.example.b"} {
		if _, ok := files[id+"/about"]; !ok {
			t.Fatalf("missing %s/about in %v", id, keys(files))
		}
	}
}

func TestBundleContinuesWhenEnrichFails(t *testing.T) {
	data := t.TempDir()
	const current = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	n, err := CreateBundle(t.Context(), data, State{Listings: []Listing{apkListing("com.example.app", current)}}, signer, 10, func(context.Context, []string) error {
		return errors.New("down")
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("epoch = %d", n)
	}
	body, err := os.ReadFile(BundlePath(data, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tarFiles(t, body)["com.example.app/about"]; ok {
		t.Fatal("shipped about without a cache")
	}
}

func TestAPKPrefersArm64(t *testing.T) {
	listing := Listing{Assets: []nostr.Event{
		{Kind: events.KindAsset, Tags: nostr.Tags{
			{"x", "aa"}, {"url", "https://example.com/x86.apk"}, {"f", "android-x86_64"},
		}},
		{Kind: events.KindAsset, Tags: nostr.Tags{
			{"x", "bb"}, {"url", "https://example.com/arm.apk"}, {"f", "android-arm64-v8a"},
		}},
	}}
	url, hash := apkOf(listing)
	if url != "https://example.com/arm.apk" || hash != "bb" {
		t.Fatalf("url %s hash %s", url, hash)
	}
}

func TestIsAndroidAPKRequiresARM64(t *testing.T) {
	if !isAndroidAPK(events.Asset{MimeType: androidAPKMIME, Platforms: []string{preferredABI}}) {
		t.Fatal("arm64 apk")
	}
	if !isAndroidAPK(events.Asset{Platforms: []string{preferredABI}}) {
		t.Fatal("arm64 without mime")
	}
	if isAndroidAPK(events.Asset{MimeType: androidAPKMIME, Platforms: []string{"android-armeabi-v7a"}}) {
		t.Fatal("v7a")
	}
	if !isAndroidAPK(events.Asset{Platforms: []string{preferredABI, "android-armeabi-v7a"}}) {
		t.Fatal("universal build")
	}
	if isAndroidAPK(events.Asset{MimeType: "application/x-executable", Platforms: []string{preferredABI}}) {
		t.Fatal("non-apk mime")
	}
}

func TestPreviousStackSignedByAnotherKeyIsDeleted(t *testing.T) {
	data := t.TempDir()
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("ab", 32)
	stack := nostr.Event{
		ID: "stack-old", Kind: events.KindStack, PubKey: other, CreatedAt: 1,
		Tags: nostr.Tags{{"d", "editors"}, {"f", "android-arm64-v8a"}},
	}
	if err := os.MkdirAll(SnapDir(data), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteSnap(SnapPath(data, 1), []nostr.Event{stack}); err != nil {
		t.Fatal(err)
	}
	n, err := CreateBundle(t.Context(), data, State{}, signer, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("epoch %d", n)
	}
	body, err := os.ReadFile(BundlePath(data, 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	diff := string(tarFiles(t, body)["diff.jsonl"])
	want := `"kind":30267,"pubkey":"` + other + `","d":"editors"`
	if !strings.Contains(diff, want) {
		t.Fatalf("diff = %s", diff)
	}
}

// resolvedListing is a listing that survives a snapshot round trip.
func resolvedListing(t *testing.T, appID string) Listing {
	t.Helper()
	pub := strings.Repeat("ab", 32)
	assetID := appID + "-asset"
	st := Resolve([]nostr.Event{
		{ID: appID + "-app", Kind: events.KindApp, PubKey: pub, Tags: nostr.Tags{{"d", appID}, {"name", appID}, {"f", preferredABI}}},
		{ID: appID + "-rel", Kind: events.KindRelease, PubKey: pub, Tags: nostr.Tags{
			{"i", appID}, {"version", "1.0.0"}, {"d", appID + "@1.0.0"}, {"c", "main"}, {"e", assetID},
		}},
		{ID: assetID, Kind: events.KindAsset, PubKey: pub, Tags: nostr.Tags{
			{"i", appID}, {"x", strings.Repeat("ef", 32)}, {"version", "1.0.0"}, {"f", preferredABI},
			{"version_code", "1"}, {"apk_certificate_hash", strings.Repeat("cd", 32)},
			{"url", "https://example.com/" + appID + ".apk"},
		}},
	}, "")
	if len(st.Listings) != 1 {
		t.Fatalf("%s: listings = %d", appID, len(st.Listings))
	}
	return st.Listings[0]
}

func apkListing(id, hash string) Listing {
	tags := nostr.Tags{{"url", "https://example.com/" + id + ".apk"}}
	if hash != "" {
		tags = append(nostr.Tags{{"x", hash}}, tags...)
	}
	return Listing{
		AppID:   id,
		App:     nostr.Event{ID: id + "-app", Kind: events.KindApp, Tags: nostr.Tags{{"d", id}}},
		Release: nostr.Event{ID: id + "-rel", Kind: events.KindRelease, Tags: nostr.Tags{{"i", id}}},
		Assets: []nostr.Event{{
			ID:   id + "-asset",
			Kind: events.KindAsset,
			Tags: append(nostr.Tags{{"i", id}}, tags...),
		}},
	}
}

func writeAbout(t *testing.T, data, id string) {
	t.Helper()
	withArtifacts(t, data, func(db *artifacts.DB) {
		if err := db.SetAbout(t.Context(), id, "sum"); err != nil {
			t.Fatal(err)
		}
	})
}

func writeCache(t *testing.T, data, id, apk string) {
	t.Helper()
	withArtifacts(t, data, func(db *artifacts.DB) {
		if err := db.SetMemo(t.Context(), id, artifacts.Memo{APK: apk}); err != nil {
			t.Fatal(err)
		}
	})
}

func withArtifacts(t *testing.T, data string, fn func(*artifacts.DB)) {
	t.Helper()
	db, err := artifacts.Open(t.Context(), artifacts.Path(data))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fn(db)
}
