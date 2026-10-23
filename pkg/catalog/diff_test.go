package catalog

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/steroid/artifacts"
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

func TestOnlyPublisherProfilesShip(t *testing.T) {
	catalog, prover, commenter := strings.Repeat("c", 64), strings.Repeat("d", 64), strings.Repeat("e", 64)
	profile := func(pub string) nostr.Event {
		return nostr.Event{ID: pub[:1], Kind: 0, PubKey: pub, CreatedAt: 1}
	}
	proof := nostr.Event{ID: "proof", Kind: 30509, PubKey: prover, CreatedAt: 1, Tags: nostr.Tags{{"d", strings.Repeat("f", 64)}}}
	raw := []nostr.Event{profile(catalog), profile(prover), profile(commenter), proof}

	var got []string
	for _, p := range Resolve(raw, catalog).Profiles {
		got = append(got, p.Pubkey)
	}
	if len(got) != 2 || got[0] != catalog || got[1] != prover {
		t.Fatalf("profiles %v, want the catalog key and the proof signer", got)
	}
	if n := len(Resolve(raw, "").Profiles); n != 3 {
		t.Fatalf("a snapshot file keeps the profiles it shipped, got %d", n)
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
	if len(diff.Events) != 2 || diff.Events[0].ID != "app" || diff.Events[1].ID != "rel-2" {
		t.Fatalf("a release update must carry the whole triple: %+v", diff.Events)
	}
}

func TestDiffOrdersListingsThenReplaceables(t *testing.T) {
	a, b := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	listing := func(id string) Listing {
		return Listing{
			AppID:   id,
			App:     nostr.Event{ID: "z-" + id, Kind: 32267},
			Release: nostr.Event{ID: "y-" + id, Kind: 30063},
			Assets:  []nostr.Event{{ID: "x2-" + id, Kind: 3063}, {ID: "x1-" + id, Kind: 3063}},
		}
	}
	gone := Profile{Pubkey: a, Event: nostr.Event{ID: "p-old", Kind: 0, PubKey: a}}
	next := State{
		Listings: []Listing{listing("com.b"), listing("com.a")},
		Proofs:   []Proof{{Pubkey: a, D: "c1", Event: nostr.Event{ID: "1", Kind: 30509, PubKey: a, Tags: nostr.Tags{{"d", "c1"}}}}},
		Profiles: []Profile{{Pubkey: b, Event: nostr.Event{ID: "9", Kind: 0, PubKey: b}}},
	}
	diff := Compare(State{Profiles: []Profile{gone}}, next)
	var got []string
	for _, event := range diff.Events {
		got = append(got, event.ID)
	}
	want := "z-com.a,y-com.a,x2-com.a,x1-com.a,z-com.b,y-com.b,x2-com.b,x1-com.b,9,1"
	if strings.Join(got, ",") != want {
		t.Fatalf("order %s", strings.Join(got, ","))
	}
	if len(diff.Coords) != 1 || diff.Coords[0] != (Coord{Kind: 0, Pubkey: a}) {
		t.Fatalf("a removed kind 0 needs a delete line: %+v", diff.Coords)
	}
}

func TestBundleCopiesArtifactDir(t *testing.T) {
	data := t.TempDir()
	avatar := strings.Repeat("ab", 32) + ".webp"
	withArtifacts(t, data, func(db *artifacts.DB) {
		if err := db.SetAbout(t.Context(), "com.example.maps", "sum"); err != nil {
			t.Fatal(err)
		}
		if err := db.SetVector(t.Context(), "com.example.maps", bytes.Repeat([]byte{1}, 768)); err != nil {
			t.Fatal(err)
		}
		pubkey := strings.TrimSuffix(avatar, ".webp")
		if err := db.SetAvatar(t.Context(), pubkey, "https://example/p", []byte("ava")); err != nil {
			t.Fatal(err)
		}
	})
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
	withArtifacts(t, data, func(db *artifacts.DB) {
		for i := range ids {
			ids[i] = fmt.Sprintf("app.%04d", i)
			if err := db.SetAbout(t.Context(), ids[i], "sum"); err != nil {
				t.Fatal(err)
			}
		}
	})
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

func TestBundleShipsEveryArtifactForAChangedApp(t *testing.T) {
	data := t.TempDir()
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	const id = "com.example.maps"
	withArtifacts(t, data, func(db *artifacts.DB) {
		if err := db.SetAbout(t.Context(), id, "Offline wallet."); err != nil {
			t.Fatal(err)
		}
		if err := db.SetSecurity(t.Context(), id, "Stays on the phone."); err != nil {
			t.Fatal(err)
		}
		if err := db.SetVector(t.Context(), id, []byte("vec")); err != nil {
			t.Fatal(err)
		}
		if err := db.SetIcon(t.Context(), id, []byte("ico")); err != nil {
			t.Fatal(err)
		}
	})
	files := tarFiles(t, mustBundle(t, data, signer, Diff{Apps: []string{id}}))
	for _, name := range artifactFiles {
		if _, ok := files[id+"/"+name]; !ok {
			t.Fatalf("missing %s in %v", name, keys(files))
		}
	}
	files = tarFiles(t, mustBundle(t, data, signer, Diff{}))
	if _, ok := files[id+"/about"]; ok {
		t.Fatal("an unchanged app shipped artifacts")
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
