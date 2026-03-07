package store

import (
	"cmp"
	"context"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	sqlite "github.com/vertex-lab/nostr-sqlite"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/events/legacy"
)

var ctx = context.Background()

func TestAppSearchQuery(t *testing.T) {
	since := nostr.Timestamp(1700000000)
	until := nostr.Timestamp(1800000000)

	tests := []struct {
		name   string
		filter nostr.Filter
		want   sqlite.Query
	}{
		{
			name: "basic search",
			filter: nostr.Filter{
				Kinds:  []int{events.KindApp},
				Search: "signal",
				Limit:  50,
			},
			want: sqlite.Query{
				SQL: `SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM events e
		JOIN apps_fts fts ON e.id = fts.id
		WHERE apps_fts MATCH ?
		ORDER BY bm25(apps_fts, 0, 20, 5, 1)
		LIMIT ?`,
				Args: []any{"\"signal\"", 50},
			},
		},
		{
			name: "search with IDs",
			filter: nostr.Filter{
				Kinds:  []int{events.KindApp},
				Search: "signal",
				IDs:    []string{"abc123", "def456"},
				Limit:  10,
			},
			want: sqlite.Query{
				SQL: `SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM events e
		JOIN apps_fts fts ON e.id = fts.id
		WHERE apps_fts MATCH ? AND e.id IN (?,?)
		ORDER BY bm25(apps_fts, 0, 20, 5, 1)
		LIMIT ?`,
				Args: []any{"\"signal\"", "abc123", "def456", 10},
			},
		},
		{
			name: "search with authors",
			filter: nostr.Filter{
				Kinds:   []int{events.KindApp},
				Search:  "signal",
				Authors: []string{"pubkey1", "pubkey2"},
				Limit:   20,
			},
			want: sqlite.Query{
				SQL: `SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM events e
		JOIN apps_fts fts ON e.id = fts.id
		WHERE apps_fts MATCH ? AND e.pubkey IN (?,?)
		ORDER BY bm25(apps_fts, 0, 20, 5, 1)
		LIMIT ?`,
				Args: []any{"\"signal\"", "pubkey1", "pubkey2", 20},
			},
		},
		{
			name: "search with since and until",
			filter: nostr.Filter{
				Kinds:  []int{events.KindApp},
				Search: "signal",
				Since:  &since,
				Until:  &until,
				Limit:  100,
			},
			want: sqlite.Query{
				SQL: `SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM events e
		JOIN apps_fts fts ON e.id = fts.id
		WHERE apps_fts MATCH ? AND e.created_at >= ? AND e.created_at <= ?
		ORDER BY bm25(apps_fts, 0, 20, 5, 1)
		LIMIT ?`,
				Args: []any{"\"signal\"", int64(1700000000), int64(1800000000), 100},
			},
		},
		{
			name: "search with tags",
			filter: nostr.Filter{
				Kinds:  []int{events.KindApp},
				Search: "signal",
				Tags:   nostr.TagMap{"t": {"productivity", "tools"}},
				Limit:  25,
			},
			want: sqlite.Query{
				SQL: `SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM events e
		JOIN apps_fts fts ON e.id = fts.id
		WHERE apps_fts MATCH ? AND EXISTS (SELECT 1 FROM tags WHERE event_id = e.id AND key = ? AND value IN (?,?))
		ORDER BY bm25(apps_fts, 0, 20, 5, 1)
		LIMIT ?`,
				Args: []any{"\"signal\"", "t", "productivity", "tools", 25},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := appSearchQuery(tt.filter)
			if err != nil {
				t.Fatalf("appSearchQuery() error = %v", err)
			}

			if len(got) != 1 {
				t.Fatalf("appSearchQuery() returned %d queries, want 1", len(got))
			}

			if got[0].SQL != tt.want.SQL {
				t.Errorf("SQL mismatch\ngot:  %q\nwant: %q", got[0].SQL, tt.want.SQL)
			}

			if !reflect.DeepEqual(got[0].Args, tt.want.Args) {
				t.Errorf("Args mismatch\ngot:  %v\nwant: %v", got[0].Args, tt.want.Args)
			}
		})
	}
}

func TestStoreQueryAppSearch(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Save multiple apps with varying relevance to "signal"
	apps := []*nostr.Event{
		{
			ID:        "app1",
			PubKey:    "pubkey1",
			CreatedAt: nostr.Timestamp(1700000001),
			Kind:      events.KindApp,
			Tags: nostr.Tags{
				{"d", "org.signal.app"},
				{"name", "Signal"},
				{"summary", "Private messenger"},
				{"f", "android-arm64-v8a"},
			},
			Content: "Signal is a privacy-focused messaging app.",
			Sig:     "sig1",
		},
		{
			ID:        "app2",
			PubKey:    "pubkey2",
			CreatedAt: nostr.Timestamp(1700000002),
			Kind:      events.KindApp,
			Tags: nostr.Tags{
				{"d", "org.telegram.messenger"},
				{"name", "Telegram"},
				{"summary", "Cloud-based messenger"},
				{"f", "android-arm64-v8a"},
			},
			Content: "Telegram is a cloud-based messaging app. Some say it's an alternative to Signal.",
			Sig:     "sig2",
		},
		{
			ID:        "app3",
			PubKey:    "pubkey3",
			CreatedAt: nostr.Timestamp(1700000003),
			Kind:      events.KindApp,
			Tags: nostr.Tags{
				{"d", "com.whatsapp"},
				{"name", "WhatsApp"},
				{"summary", "Popular messenger"},
				{"f", "android-arm64-v8a"},
			},
			Content: "WhatsApp is a popular messaging application.",
			Sig:     "sig3",
		},
		{
			ID:        "app4",
			PubKey:    "pubkey4",
			CreatedAt: nostr.Timestamp(1700000004),
			Kind:      events.KindApp,
			Tags: nostr.Tags{
				{"d", "com.signal.signaling"},
				{"name", "Signal Signaling"},
				{"summary", "Signal Signaling is the signaling protocol for Signal."},
				{"f", "android-x86"}, // different platform, so it should not be returned
			},
			Content: "Signal Signaling is the signaling protocol for Signal.",
			Sig:     "sig4",
		},
	}

	for _, app := range apps {
		if _, err := store.Save(ctx, app); err != nil {
			t.Fatalf("failed to save app %s: %v", app.ID, err)
		}
	}

	// Query for "signal"
	filter := nostr.Filter{
		Kinds:  []int{events.KindApp},
		Search: "signal",
		Tags:   nostr.TagMap{"f": {"android-arm64-v8a"}},
		Limit:  50,
	}

	results, err := store.Query(ctx, filter)
	if err != nil {
		t.Fatalf("store.Query() error = %v", err)
	}

	expected := []nostr.Event{*apps[0], *apps[1]}
	if !reflect.DeepEqual(results, expected) {
		t.Errorf("results mismatch\ngot:  %v\nwant: %v", results, expected)
	}
}

// Indexed tag keys per event kind
var (
	appIndexedKeys     = []string{"d", "name", "t", "f", "license", "url", "repository", "a"}
	releaseIndexedKeys = []string{"d", "i", "version", "c", "e", "a", "commit"}
	assetIndexedKeys   = []string{"i", "x", "f", "m", "url", "version", "apk_certificate_hash"}
	fileIndexedKeys    = []string{"x", "f", "m", "url", "fallback", "version", "apk_signature_hash"}
)

func TestAppTagsIndexing(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	event := &nostr.Event{
		ID:        "app123",
		PubKey:    "pubkey123",
		CreatedAt: nostr.Timestamp(1700000000),
		Kind:      events.KindApp,
		Tags: nostr.Tags{
			{"d", "com.example.app"},
			{"name", "Example App"},
			{"f", "android-arm64-v8a"},
			{"f", "linux-x86_64"},
			{"summary", "A short description"},               // FTS only, not in tags
			{"icon", "https://example.com/icon.png"},         // Not indexed
			{"image", "https://example.com/screenshot1.png"}, // Not indexed
			{"t", "productivity"},
			{"t", "tools"},
			{"url", "https://example.com"},
			{"repository", "https://github.com/example/app"},
			{"license", "MIT"},
		},
		Content: "Full app description",
		Sig:     "sig123",
	}

	saved, err := store.Save(ctx, event)
	if err != nil {
		t.Fatalf("failed to save event: %v", err)
	}
	if !saved {
		t.Fatal("event was not saved")
	}

	got := getIndexedTags(t, store, event.ID)
	want := expectedTags(event, appIndexedKeys)

	if !equalTags(got, want) {
		t.Errorf("indexed tags mismatch\ngot:  %v\nwant: %v", got, want)
	}
}

func TestAppFTSIndexing(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	event := &nostr.Event{
		ID:        "app123",
		PubKey:    "pubkey123",
		CreatedAt: nostr.Timestamp(1700000000),
		Kind:      events.KindApp,
		Tags: nostr.Tags{
			{"d", "com.example.app"},
			{"name", "Signal Messenger"},
			{"summary", "Private messaging app"},
			{"f", "android-arm64-v8a"},
		},
		Content: "Signal is a privacy-focused messaging application with end-to-end encryption.",
		Sig:     "sig123",
	}

	if _, err := store.Save(ctx, event); err != nil {
		t.Fatalf("failed to save event: %v", err)
	}

	// Verify FTS entry exists
	var eventID, name, summary, content string
	err = store.DB.QueryRowContext(ctx,
		"SELECT id, name, summary, content FROM apps_fts WHERE id = ?",
		event.ID,
	).Scan(&eventID, &name, &summary, &content)
	if err != nil {
		t.Fatalf("failed to query apps_fts: %v", err)
	}

	if eventID != event.ID {
		t.Errorf("id mismatch: got %q, want %q", eventID, event.ID)
	}
	if name != "Signal Messenger" {
		t.Errorf("name mismatch: got %q, want %q", name, "Signal Messenger")
	}
	if summary != "Private messaging app" {
		t.Errorf("summary mismatch: got %q, want %q", summary, "Private messaging app")
	}
	if content != event.Content {
		t.Errorf("content mismatch: got %q, want %q", content, event.Content)
	}

	deleted, err := store.Delete(ctx, event.ID)
	if err != nil {
		t.Fatalf("failed to delete event: %v", err)
	}
	if !deleted {
		t.Fatal("event was not deleted")
	}

	// Verify FTS entry is cleaned up
	var count int
	err = store.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM apps_fts WHERE id = ?",
		event.ID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("failed to query apps_fts: %v", err)
	}
	if count != 0 {
		t.Errorf("FTS entry not cleaned up: got %d entries, want 0", count)
	}
}

func TestReleaseTagsIndexing(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	event := &nostr.Event{
		ID:        "release123",
		PubKey:    "pubkey123",
		CreatedAt: nostr.Timestamp(1700000000),
		Kind:      events.KindRelease,
		Tags: nostr.Tags{
			{"d", "com.example.app@1.0.0"},
			{"i", "com.example.app"},
			{"version", "1.0.0"},
			{"c", "stable"},
			{"e", "asset123"},
			{"e", "asset456"},
			{"a", "32267:pubkey123:com.example.app"},
			{"commit", "abc123def456"},
		},
		Content: "Release notes",
		Sig:     "sig123",
	}

	saved, err := store.Save(ctx, event)
	if err != nil {
		t.Fatalf("failed to save event: %v", err)
	}
	if !saved {
		t.Fatal("event was not saved")
	}

	got := getIndexedTags(t, store, event.ID)
	want := expectedTags(event, releaseIndexedKeys)

	if !equalTags(got, want) {
		t.Errorf("indexed tags mismatch\ngot:  %v\nwant: %v", got, want)
	}
}

func TestAssetTagsIndexing(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	event := &nostr.Event{
		ID:        "asset123",
		PubKey:    "pubkey123",
		CreatedAt: nostr.Timestamp(1700000000),
		Kind:      events.KindAsset,
		Tags: nostr.Tags{
			{"i", "com.example.app"},
			{"x", "abc123def456"},
			{"version", "1.0.0"},
			{"f", "android-arm64-v8a"},
			{"f", "android-armeabi-v7a"},
			{"url", "https://cdn.example.com/app.apk"},
			{"m", "application/vnd.android.package-archive"},
			{"apk_certificate_hash", "hash123"},
			{"apk_certificate_hash", "hash456"},
			{"size", "12345678"},    // Not indexed
			{"version_code", "100"}, // Not indexed
		},
		Content: "",
		Sig:     "sig123",
	}

	saved, err := store.Save(ctx, event)
	if err != nil {
		t.Fatalf("failed to save event: %v", err)
	}
	if !saved {
		t.Fatal("event was not saved")
	}

	got := getIndexedTags(t, store, event.ID)
	want := expectedTags(event, assetIndexedKeys)

	if !equalTags(got, want) {
		t.Errorf("indexed tags mismatch\ngot:  %v\nwant: %v", got, want)
	}
}

func TestFileTagsIndexing(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	event := &nostr.Event{
		ID:        "file123",
		PubKey:    "pubkey123",
		CreatedAt: nostr.Timestamp(1700000000),
		Kind:      legacy.KindFile,
		Tags: nostr.Tags{
			{"x", "abc123def456"},
			{"url", "https://cdn.example.com/app.apk"},
			{"fallback", "https://backup.example.com/app.apk"},
			{"m", "application/vnd.android.package-archive"},
			{"version", "1.0.0"},
			{"f", "android-arm64-v8a"},
			{"f", "android-armeabi-v7a"},
			{"apk_signature_hash", "sighash123"},
			{"version_code", "100"},      // Not indexed
			{"min_sdk_version", "21"},    // Not indexed
			{"target_sdk_version", "34"}, // Not indexed
		},
		Content: "",
		Sig:     "sig123",
	}

	saved, err := store.Save(ctx, event)
	if err != nil {
		t.Fatalf("failed to save event: %v", err)
	}
	if !saved {
		t.Fatal("event was not saved")
	}

	got := getIndexedTags(t, store, event.ID)
	want := expectedTags(event, fileIndexedKeys)

	if !equalTags(got, want) {
		t.Errorf("indexed tags mismatch\ngot:  %v\nwant: %v", got, want)
	}
}

// getIndexedTags returns all tags indexed for an event from the tags table,
// sorted in lexicographic order by key then value.
func getIndexedTags(t *testing.T, store *sqlite.Store, eventID string) nostr.Tags {
	t.Helper()
	rows, err := store.DB.Query("SELECT key, value FROM tags WHERE event_id = ?", eventID)
	if err != nil {
		t.Fatalf("failed to query tags: %v", err)
	}
	defer rows.Close()

	var tags nostr.Tags
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatalf("failed to scan tag row: %v", err)
		}
		tags = append(tags, nostr.Tag{key, value})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("error iterating tag rows: %v", err)
	}

	sortTags(tags)
	return tags
}

func BenchmarkSaveApp(b *testing.B) {
	path := b.TempDir() + "/test.db"
	store, err := New(path)
	if err != nil {
		b.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	baseTags := nostr.Tags{
		{"d", "com.example.app"},
		{"name", "Example App"},
		{"f", "android-arm64-v8a"},
		{"f", "linux-x86_64"},
		{"summary", "A short description"},
		{"icon", "https://example.com/icon.png"},
		{"image", "https://example.com/screenshot1.png"},
		{"t", "productivity"},
		{"t", "tools"},
		{"url", "https://example.com"},
		{"repository", "https://github.com/example/app"},
		{"license", "MIT"},
	}

	toSave := make([]*nostr.Event, b.N)
	for i := range b.N {
		tags := make(nostr.Tags, len(baseTags))
		copy(tags, baseTags)

		toSave[i] = &nostr.Event{
			ID:        "bench-app-" + strconv.Itoa(i),
			PubKey:    "pubkey-bench-" + strconv.Itoa(i),
			CreatedAt: nostr.Timestamp(1700000000 + int64(i)),
			Kind:      events.KindApp,
			Tags:      tags,
			Content:   "Benchmarking app save performance.",
			Sig:       "sig",
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := range b.N {
		if _, err := store.Replace(ctx, toSave[i]); err != nil {
			b.Fatalf("failed to save event %s: %v", toSave[i], err)
		}
	}
}

// expectedTags extracts tags from an event that match the given indexed keys.
// Returns tags sorted in lexicographic order by key then value.
func expectedTags(event *nostr.Event, indexedKeys []string) nostr.Tags {
	var tags nostr.Tags
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}

		if slices.Contains(indexedKeys, tag[0]) {
			tags = append(tags, nostr.Tag{tag[0], tag[1]})
		}
	}
	sortTags(tags)
	return tags
}

// sortTags sorts tags in lexicographic order by key then value.
func sortTags(tags nostr.Tags) {
	slices.SortFunc(tags, func(a, b nostr.Tag) int {
		if c := cmp.Compare(a[0], b[0]); c != 0 {
			return c
		}
		return cmp.Compare(a[1], b[1])
	})
}

// equalTags compares two nostr.Tags for equality.
func equalTags(a, b nostr.Tags) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !slices.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
