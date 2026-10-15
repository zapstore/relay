package store

import (
	"cmp"
	"context"
	"errors"
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

func TestSearchQuery(t *testing.T) {
	got, err := searchQuery(nostr.Filter{
		Kinds:  []int{events.KindApp},
		Search: "https://github.com/example/app.git",
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("searchQuery() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("searchQuery() returned %d queries, want 1", len(got))
	}
	want := sqlite.Query{
		SQL: `SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM events e
		JOIN tags t ON t.event_id = e.id
		WHERE e.kind = 32267
		  AND t.key = 'repository'
		  AND (t.value = ? OR t.value = ?)
		LIMIT ?`,
		Args: []any{"https://github.com/example/app", "https://github.com/example/app.git", 10},
	}
	if got[0].SQL != want.SQL {
		t.Errorf("SQL mismatch\ngot:  %q\nwant: %q", got[0].SQL, want.SQL)
	}
	if !reflect.DeepEqual(got[0].Args, want.Args) {
		t.Errorf("Args mismatch\ngot:  %v\nwant: %v", got[0].Args, want.Args)
	}

	if _, err := searchQuery(nostr.Filter{Kinds: []int{events.KindApp}, Search: "signal"}); !errors.Is(err, ErrUnsupportedREQ) {
		t.Fatalf("text search error = %v, want ErrUnsupportedREQ", err)
	}
}

func TestValidateSearch(t *testing.T) {
	repo := nostr.Filter{Kinds: []int{events.KindApp}, Search: "github.com/example/app"}
	if err := Validate(repo); err != nil {
		t.Fatalf("repository URL: %v", err)
	}
	if err := Validate(nostr.Filter{Kinds: []int{events.KindApp}, Search: "signal"}); !errors.Is(err, ErrUnsupportedREQ) {
		t.Fatalf("text search error = %v, want ErrUnsupportedREQ", err)
	}
	if err := Validate(repo, repo); !errors.Is(err, ErrUnsupportedREQ) {
		t.Fatalf("two search filters error = %v, want ErrUnsupportedREQ", err)
	}
}

func TestStoreQueryRepository(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	app := &nostr.Event{
		ID:        "app1",
		PubKey:    "pubkey1",
		CreatedAt: nostr.Timestamp(1700000001),
		Kind:      events.KindApp,
		Tags: nostr.Tags{
			{"d", "org.signal.app"},
			{"name", "Signal"},
			{"repository", "https://github.com/signalapp/Signal-Android.git"},
		},
		Content: "Signal is a privacy-focused messaging app.",
		Sig:     "sig1",
	}
	other := &nostr.Event{
		ID:        "app2",
		PubKey:    "pubkey2",
		CreatedAt: nostr.Timestamp(1700000002),
		Kind:      events.KindApp,
		Tags: nostr.Tags{
			{"d", "org.telegram.messenger"},
			{"name", "Telegram"},
			{"repository", "https://github.com/telegramdesktop/tdesktop"},
		},
		Content: "Telegram.",
		Sig:     "sig2",
	}
	for _, event := range []*nostr.Event{app, other} {
		if _, err := store.Save(ctx, event); err != nil {
			t.Fatalf("failed to save app %s: %v", event.ID, err)
		}
	}

	results, err := store.Query(ctx, nostr.Filter{
		Kinds:  []int{events.KindApp},
		Search: "github.com/signalapp/Signal-Android",
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("store.Query() error = %v", err)
	}
	if !reflect.DeepEqual(results, []nostr.Event{*app}) {
		t.Errorf("results mismatch\ngot:  %v\nwant: %v", results, []nostr.Event{*app})
	}

	if _, err := store.Query(ctx, nostr.Filter{
		Kinds:  []int{events.KindApp},
		Search: "signal",
		Limit:  10,
	}); !errors.Is(err, ErrUnsupportedREQ) {
		t.Fatalf("text search error = %v, want ErrUnsupportedREQ", err)
	}
}

// Multi-character tag keys indexed per event kind (kind-specific triggers).
// All single-letter tags are indexed universally by single_letter_tags_ai.
var (
	appMultiCharKeys     = []string{"name", "license", "url", "repository"}
	releaseMultiCharKeys = []string{"version", "commit"}
	assetMultiCharKeys   = []string{"url", "version", "apk_certificate_hash", "apk_certificate_ancestor"}
	fileMultiCharKeys    = []string{"url", "fallback", "version", "apk_signature_hash"}
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
			{"summary", "A short description"},               // not indexed
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
	want := expectedTags(event, appMultiCharKeys)

	if !equalTags(got, want) {
		t.Errorf("indexed tags mismatch\ngot:  %v\nwant: %v", got, want)
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
	want := expectedTags(event, releaseMultiCharKeys)

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
	want := expectedTags(event, assetMultiCharKeys)

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
	want := expectedTags(event, fileMultiCharKeys)

	if !equalTags(got, want) {
		t.Errorf("indexed tags mismatch\ngot:  %v\nwant: %v", got, want)
	}
}

func TestCommentTagsIndexing(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	event := &nostr.Event{
		ID:        "comment123",
		PubKey:    "aa1f96f685d0ac3e28a52feb87a20399a91afb3ac3137afeb7698dfcc99bc454",
		CreatedAt: nostr.Timestamp(1700000000),
		Kind:      events.KindComment,
		Tags: nostr.Tags{
			{"A", "32267:963a2dca29e2ed5663a627b5289ed36d445531a3f5ef127716227d8a1aaa5166:com.mempoolapp"},
			{"K", "32267"},
			{"P", "963a2dca29e2ed5663a627b5289ed36d445531a3f5ef127716227d8a1aaa5166"},
			{"e", "0d30205124547672332e64a6fa29d19a63d3ee68f62862294e17ad31caa4e341"},
			{"k", "1111"},
			{"p", "963a2dca29e2ed5663a627b5289ed36d445531a3f5ef127716227d8a1aaa5166"},
			{"v", "1.0.14"},
		},
		Content: "great app",
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
	want := expectedTags(event, nil)

	if !equalTags(got, want) {
		t.Errorf("indexed tags mismatch\ngot:  %v\nwant: %v", got, want)
	}

	// Verify queryable by A tag
	results, err := store.Query(ctx, nostr.Filter{
		Kinds: []int{events.KindComment},
		Tags:  nostr.TagMap{"A": {"32267:963a2dca29e2ed5663a627b5289ed36d445531a3f5ef127716227d8a1aaa5166:com.mempoolapp"}},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("query by A tag: %v", err)
	}
	if len(results) != 1 || results[0].ID != event.ID {
		t.Errorf("query by A tag: want 1 result with ID %q, got %d results", event.ID, len(results))
	}

	// Verify queryable by e tag
	results, err = store.Query(ctx, nostr.Filter{
		Kinds: []int{events.KindComment},
		Tags:  nostr.TagMap{"e": {"0d30205124547672332e64a6fa29d19a63d3ee68f62862294e17ad31caa4e341"}},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("query by e tag: %v", err)
	}
	if len(results) != 1 || results[0].ID != event.ID {
		t.Errorf("query by e tag: want 1 result with ID %q, got %d results", event.ID, len(results))
	}
}

func TestZapTagsIndexing(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	event := &nostr.Event{
		ID:        "zap123",
		PubKey:    "pubkey123",
		CreatedAt: nostr.Timestamp(1700000000),
		Kind:      events.KindZap,
		Tags: nostr.Tags{
			{"a", "32267:963a2dca29e2ed5663a627b5289ed36d445531a3f5ef127716227d8a1aaa5166:com.mempoolapp"},
			{"p", "963a2dca29e2ed5663a627b5289ed36d445531a3f5ef127716227d8a1aaa5166"},
			{"e", "targetid123"},
			{"P", "senderpubkey"},
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
	want := expectedTags(event, nil)

	if !equalTags(got, want) {
		t.Errorf("indexed tags mismatch\ngot:  %v\nwant: %v", got, want)
	}

	// Verify queryable by a tag
	results, err := store.Query(ctx, nostr.Filter{
		Kinds: []int{events.KindZap},
		Tags:  nostr.TagMap{"a": {"32267:963a2dca29e2ed5663a627b5289ed36d445531a3f5ef127716227d8a1aaa5166:com.mempoolapp"}},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("query by a tag: %v", err)
	}
	if len(results) != 1 || results[0].ID != event.ID {
		t.Errorf("query by a tag: want 1 result with ID %q, got %d results", event.ID, len(results))
	}
}

func TestPendingEvents(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	makeEvent := func(id string, kind int) nostr.Event {
		return nostr.Event{
			ID:        id,
			PubKey:    "pubkey",
			CreatedAt: nostr.Timestamp(1700000000),
			Kind:      kind,
			Tags:      nostr.Tags{{"d", id}},
			Content:   "content-" + id,
			Sig:       "sig",
		}
	}

	asset1 := makeEvent("asset1", events.KindAsset)
	asset2 := makeEvent("asset2", events.KindAsset)
	release1 := makeEvent("release1", events.KindRelease)

	for _, e := range []nostr.Event{asset1, asset2, release1} {
		if _, err := store.SavePending(ctx, &e); err != nil {
			t.Fatalf("SavePending(%s): %v", e.ID, err)
		}
	}

	got, err := store.QueryPending(ctx, events.KindAsset)
	if err != nil {
		t.Fatalf("QueryPending: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 pending assets, got %d", len(got))
	}

	// Index by ID for order-independent comparison.
	byID := make(map[string]nostr.Event, len(got))
	for _, e := range got {
		byID[e.ID] = e
	}
	for _, want := range []nostr.Event{asset1, asset2} {
		got, ok := byID[want.ID]
		if !ok {
			t.Errorf("missing event %s in QueryPending result", want.ID)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("event %s mismatch:\ngot:  %+v\nwant: %+v", want.ID, got, want)
		}
	}
}

func TestForceDeleteRequest(t *testing.T) {
	const (
		alice = "alice"
		bob   = "bob"
	)

	tests := []struct {
		name    string
		stored  []nostr.Event
		request nostr.Event
		err     error
		deleted int
	}{
		{
			name:    "wrong kind",
			request: nostr.Event{Kind: 1},
			err:     sqlite.ErrInvalidDeletionRequest,
		},
		{
			name:    "no e or a tags",
			request: nostr.Event{Kind: 5, PubKey: alice},
		},

		// e-tag: unlike a normal NIP-09 deletion, ForceDeleteRequest must delete
		// referenced events regardless of whether the requester's pubkey matches.
		{
			name: "e tag: deletes own event",
			stored: []nostr.Event{
				{ID: "event1", PubKey: alice, Kind: 1},
			},
			request: nostr.Event{
				Kind:   5,
				PubKey: alice,
				Tags:   nostr.Tags{{"e", "event1"}},
			},
			deleted: 1,
		},
		{
			name: "e tag: deletes event with different pubkey (force, ignores ownership)",
			stored: []nostr.Event{
				{ID: "event1", PubKey: bob, Kind: 1},
			},
			request: nostr.Event{
				Kind:   5,
				PubKey: alice,
				Tags:   nostr.Tags{{"e", "event1"}},
			},
			deleted: 1,
		},
		{
			name: "e tag: event not found",
			request: nostr.Event{
				Kind:   5,
				PubKey: alice,
				Tags:   nostr.Tags{{"e", "nonexistent"}},
			},
			deleted: 0,
		},
		{
			name: "multiple e tags: all deleted across different owners",
			stored: []nostr.Event{
				{ID: "e1", PubKey: alice, Kind: 1},
				{ID: "e2", PubKey: bob, Kind: 1},
			},
			request: nostr.Event{
				Kind:   5,
				PubKey: alice,
				Tags:   nostr.Tags{{"e", "e1"}, {"e", "e2"}},
			},
			deleted: 2,
		},
		{
			name: "multiple e tags: partial match",
			stored: []nostr.Event{
				{ID: "e1", PubKey: alice, Kind: 1},
			},
			request: nostr.Event{
				Kind:   5,
				PubKey: alice,
				Tags:   nostr.Tags{{"e", "e1"}, {"e", "nonexistent"}},
			},
			deleted: 1,
		},

		// a-tag: ForceDeleteRequest must delete ALL versions of an addressable event,
		// with no created_at cutoff, and regardless of who issued the deletion request.
		{
			name: "a tag: deletes addressable event",
			stored: []nostr.Event{
				{ID: "addr1", PubKey: alice, Kind: 30000, Tags: nostr.Tags{{"d", "test"}}, CreatedAt: 100},
			},
			request: nostr.Event{
				Kind:      5,
				PubKey:    alice,
				CreatedAt: 200,
				Tags:      nostr.Tags{{"a", "30000:alice:test"}},
			},
			deleted: 1,
		},
		{
			name: "a tag: deletes version newer than deletion request (force, no created_at cutoff)",
			stored: []nostr.Event{
				{ID: "addr1", PubKey: alice, Kind: 30000, Tags: nostr.Tags{{"d", "test"}}, CreatedAt: 300},
			},
			request: nostr.Event{
				Kind:      5,
				PubKey:    alice,
				CreatedAt: 200,
				Tags:      nostr.Tags{{"a", "30000:alice:test"}},
			},
			deleted: 1,
		},
		{
			name: "a tag: deletes event whose coordinate pubkey differs from requester (force)",
			stored: []nostr.Event{
				{ID: "addr1", PubKey: bob, Kind: 30000, Tags: nostr.Tags{{"d", "test"}}, CreatedAt: 100},
			},
			request: nostr.Event{
				Kind:      5,
				PubKey:    alice,
				CreatedAt: 200,
				Tags:      nostr.Tags{{"a", "30000:bob:test"}},
			},
			deleted: 1,
		},
		{
			name: "a tag: all versions deleted regardless of created_at",
			stored: []nostr.Event{
				{ID: "v1", PubKey: alice, Kind: 30000, Tags: nostr.Tags{{"d", "doc"}}, CreatedAt: 50},
				{ID: "v2", PubKey: alice, Kind: 30000, Tags: nostr.Tags{{"d", "doc"}}, CreatedAt: 150},
				{ID: "v3", PubKey: alice, Kind: 30000, Tags: nostr.Tags{{"d", "doc"}}, CreatedAt: 250},
			},
			request: nostr.Event{
				Kind:      5,
				PubKey:    alice,
				CreatedAt: 200,
				Tags:      nostr.Tags{{"a", "30000:alice:doc"}},
			},
			deleted: 3,
		},
		{
			name: "a tag: malformed (missing separators)",
			request: nostr.Event{
				Kind:      5,
				PubKey:    alice,
				CreatedAt: 200,
				Tags:      nostr.Tags{{"a", "notvalid"}},
			},
			deleted: 0,
		},
		{
			name: "a tag: malformed (non-integer kind)",
			request: nostr.Event{
				Kind:      5,
				PubKey:    alice,
				CreatedAt: 200,
				Tags:      nostr.Tags{{"a", "notakind:alice:test"}},
			},
			deleted: 0,
		},

		// mixed: both e and a tags in a single deletion request, across different owners.
		{
			name: "mixed e and a tags: cross-pubkey deletions",
			stored: []nostr.Event{
				{ID: "regular1", PubKey: bob, Kind: 1, CreatedAt: 100},
				{ID: "addr1", PubKey: bob, Kind: 30000, Tags: nostr.Tags{{"d", "home"}}, CreatedAt: 300},
			},
			request: nostr.Event{
				Kind:      5,
				PubKey:    alice,
				CreatedAt: 200,
				Tags:      nostr.Tags{{"e", "regular1"}, {"a", "30000:bob:home"}},
			},
			deleted: 2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := New(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			for i := range test.stored {
				if _, err := store.Save(ctx, &test.stored[i]); err != nil {
					t.Fatalf("Save failed: %v", err)
				}
			}

			deleted, err := store.ForceDeleteRequest(ctx, &test.request)
			if !errors.Is(err, test.err) {
				t.Fatalf("expected error %v, got %v", test.err, err)
			}
			if deleted != test.deleted {
				t.Fatalf("expected %d deleted, got %d", test.deleted, deleted)
			}
		})
	}
}

// getIndexedTags returns all tags indexed for an event from the tags table,
// sorted in lexicographic order by key then value.
func getIndexedTags(t *testing.T, store T, eventID string) nostr.Tags {
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

// expectedTags extracts tags from an event that should be indexed:
// all single-letter tag keys (universal trigger) plus the given multi-char keys (kind-specific trigger).
// Returns tags sorted in lexicographic order by key then value.
func expectedTags(event *nostr.Event, multiCharKeys []string) nostr.Tags {
	var tags nostr.Tags
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}

		isSingleLetter := len(tag[0]) == 1
		if isSingleLetter || slices.Contains(multiCharKeys, tag[0]) {
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
