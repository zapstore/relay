package catalog

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/nbd-wtf/go-nostr"
)

func TestBanLineFollowsDelete(t *testing.T) {
	raw, err := MarshalDiff(Diff{
		AppDeletes: []string{"com.example.spy"},
		Notices:    map[string]string{"com.example.spy": "Records the microphone and uploads it."},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, `"delete":["com.example.spy"]`) || !strings.Contains(got, `"banned":true`) || !strings.Contains(got, `"reason":"Records the microphone and uploads it."`) {
		t.Fatalf("diff %s", got)
	}
}

func TestDiffDropsAppAndKeepsReplacement(t *testing.T) {
	prev := State{Listings: []Listing{{
		AppID:   "com.example.beacon",
		App:     nostr.Event{ID: "app-b", Kind: 32267},
		Release: nostr.Event{ID: "rel-b", Kind: 30063},
		Assets:  []nostr.Event{{ID: "asset-b", Kind: 3063}},
	}, {
		AppID:   "com.example.maps",
		App:     nostr.Event{ID: "app-m", Kind: 32267},
		Release: nostr.Event{ID: "rel-1", Kind: 30063},
		Assets:  []nostr.Event{{ID: "asset-1", Kind: 3063}},
	}}}
	next := State{Listings: []Listing{{
		AppID:   "com.example.maps",
		App:     nostr.Event{ID: "app-m", Kind: 32267},
		Release: nostr.Event{ID: "rel-2", Kind: 30063},
		Assets:  []nostr.Event{{ID: "asset-2", Kind: 3063}},
	}}}
	diff := Compare(prev, next)
	if len(diff.AppDeletes) != 1 || diff.AppDeletes[0] != "com.example.beacon" {
		t.Fatalf("deletes = %v", diff.AppDeletes)
	}
	if len(diff.Apps) != 1 || diff.Apps[0] != "com.example.maps" {
		t.Fatalf("apps = %v", diff.Apps)
	}
	raw, err := MarshalDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"delete":["com.example.beacon"]`)) {
		t.Fatalf("diff = %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"id":"rel-2"`)) {
		t.Fatalf("missing release: %s", raw)
	}
}

func TestKind0FollowsReplaceableKey(t *testing.T) {
	pubkey := strings.Repeat("ab", 32)
	older := nostr.Event{ID: "b", Kind: 0, PubKey: pubkey, CreatedAt: 1, Content: `{"picture":"https://cdn.example/a.png"}`}
	newer := nostr.Event{ID: "a", Kind: 0, PubKey: pubkey, CreatedAt: 2, Content: `{"picture":"https://cdn.example/b.png"}`}
	app := nostr.Event{ID: "app", Kind: 32267, PubKey: pubkey, CreatedAt: 1, Tags: nostr.Tags{{"d", "com.example"}}}
	next := Resolve([]nostr.Event{older, newer, app}, "")
	if len(next.Profiles) != 1 || next.Profiles[0].Event.ID != "a" || next.Profiles[0].Picture != "https://cdn.example/b.png" {
		t.Fatalf("%+v", next.Profiles)
	}
	diff := Compare(State{Profiles: []Profile{{Pubkey: pubkey, Event: newer}}}, next)
	for _, event := range diff.Events {
		if event.Kind == 0 {
			t.Fatalf("unchanged kind 0 in diff: %+v", event)
		}
	}
	release := nostr.Event{ID: "rel-2", Kind: 30063, PubKey: pubkey, CreatedAt: 3, Tags: nostr.Tags{{"d", "com.example@2"}, {"i", "com.example"}}}
	withRelease := next
	withRelease.Listings = []Listing{{AppID: "com.example", App: app, Release: release}}
	diff = Compare(State{Profiles: next.Profiles, Listings: []Listing{{AppID: "com.example", App: app, Release: nostr.Event{ID: "rel-1", Kind: 30063}}}}, withRelease)
	for _, event := range diff.Events {
		if event.Kind == 0 {
			t.Fatalf("kind 0 repeated for a release: %+v", event)
		}
	}
	if len(diff.Events) != 1 || diff.Events[0].ID != "rel-2" {
		t.Fatalf("events %+v", diff.Events)
	}
}

func TestBundleCopiesArtifactDir(t *testing.T) {
	data := t.TempDir()
	dir := AppDir(data, "com.example.maps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "about"), []byte("sum\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vector"), bytes.Repeat([]byte{1}, 768), 0o644); err != nil {
		t.Fatal(err)
	}
	avatar := strings.Repeat("ab", 32) + ".webp"
	if err := os.WriteFile(filepath.Join(ArtifactDir(data), avatar), []byte("ava"), 0o644); err != nil {
		t.Fatal(err)
	}
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	diff := Diff{
		Events: []nostr.Event{{
			ID: "app-m", Kind: 32267, PubKey: signer.PubKey, CreatedAt: 1,
			Tags: nostr.Tags{{"d", "com.example.maps"}},
		}},
		Apps:    []string{"com.example.maps"},
		Avatars: []string{avatar},
	}
	body, err := BuildBundle(t.Context(), data, 0, 1, 10, diff, signer)
	if err != nil {
		t.Fatal(err)
	}
	files := tarFiles(t, body)
	for _, want := range []string{
		"manifest.json",
		"index",
		"diff.jsonl",
		"com.example.maps/about",
		"com.example.maps/vector",
		avatar,
	} {
		if _, ok := files[want]; !ok {
			t.Fatalf("missing %s in %v", want, keys(files))
		}
	}
	if bytes.Contains(files["manifest.json"], []byte(`"r"`)) {
		t.Fatalf("manifest contains a relay URL: %s", files["manifest.json"])
	}
}

func TestLargeBundleManifestFitsBunker(t *testing.T) {
	data := t.TempDir()
	const apps = 4080
	ids := make([]string, apps)
	for i := range ids {
		ids[i] = fmt.Sprintf("app.%04d", i)
		dir := AppDir(data, ids[i])
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "about"), []byte("sum\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	events := make([]nostr.Event, apps)
	for i, id := range ids {
		events[i] = nostr.Event{ID: id, Kind: 32267, Tags: nostr.Tags{{"d", id}}}
	}
	body, err := BuildBundle(t.Context(), data, 0, 1, 10, Diff{Apps: ids, Events: events}, signer)
	if err != nil {
		t.Fatal(err)
	}
	files := tarFiles(t, body)
	manifest := files["manifest.json"]
	if len(manifest) > 8<<10 {
		t.Fatalf("manifest is %d bytes", len(manifest))
	}
	if bytes.Count(manifest, []byte(`"file"`)) != 1 {
		t.Fatalf("manifest tags: %s", manifest)
	}
	lines := bytes.Count(files["index"], []byte("\n"))
	if lines != apps+1 {
		t.Fatalf("index lines %d", lines)
	}
}

func TestBundleFilesFollowEventKind(t *testing.T) {
	data := t.TempDir()
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	const id = "com.example.maps"
	dir := AppDir(data, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"about": []byte("Offline wallet.\n"), "security": []byte("Stays on the phone.\n"),
		"facts":  []byte("\"offline_capable\",\"yes\",\"No INTERNET\"\n"),
		"vector": []byte("vec"), "icon.webp": []byte("ico"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	listing := nostr.Event{ID: "app", Kind: 32267, Tags: nostr.Tags{{"d", id}}}
	asset := nostr.Event{ID: "apk", Kind: 3063, Tags: nostr.Tags{{"i", id}}}
	release := nostr.Event{ID: "rel", Kind: 30063, Tags: nostr.Tags{{"i", id}}}

	onlyRelease := tarFiles(t, mustBundle(t, data, signer, Diff{Apps: []string{id}, Events: []nostr.Event{release}}))
	for _, name := range []string{"about", "vector", "icon.webp", "security", "facts"} {
		if _, ok := onlyRelease[id+"/"+name]; ok {
			t.Fatalf("release shipped %s", name)
		}
	}
	onlyApp := tarFiles(t, mustBundle(t, data, signer, Diff{Apps: []string{id}, Events: []nostr.Event{listing}}))
	for _, name := range []string{"about", "vector", "icon.webp"} {
		if _, ok := onlyApp[id+"/"+name]; !ok {
			t.Fatalf("missing %s", name)
		}
	}
	for _, name := range []string{"security", "facts"} {
		if _, ok := onlyApp[id+"/"+name]; ok {
			t.Fatalf("app event shipped %s", name)
		}
	}
	onlyAsset := tarFiles(t, mustBundle(t, data, signer, Diff{Apps: []string{id}, Events: []nostr.Event{asset}}))
	if _, ok := onlyAsset[id+"/security"]; !ok {
		t.Fatal("missing security")
	}
	if _, ok := onlyAsset[id+"/facts"]; ok {
		t.Fatal("shipped facts")
	}
	for _, name := range []string{"about", "vector", "icon.webp"} {
		if _, ok := onlyAsset[id+"/"+name]; ok {
			t.Fatalf("asset shipped %s", name)
		}
	}
}

func mustBundle(t *testing.T, data string, signer Signer, diff Diff) []byte {
	t.Helper()
	body, err := BuildBundle(t.Context(), data, 0, 1, 10, diff, signer)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func tarFiles(t *testing.T, body []byte) map[string][]byte {
	t.Helper()
	zr, err := zstd.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		buf, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = buf
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
