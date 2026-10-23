package relay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/relay/store"
)

func TestSelectSlots(t *testing.T) {
	const appID = "com.example.app"
	cert := strings.Repeat("cd", 32)
	catalog := testPubkey(t)
	developer := testPubkey(t)
	solo := "com.example.solo"

	setup := func(t *testing.T, catalogCode string) store.T {
		db := newSlotDB(t)
		saveListing(t, db, catalog, appID, cert, "1", catalogCode)
		saveListing(t, db, developer, appID, cert, "2", "2")
		saveListing(t, db, catalog, solo, cert, "3", "1")
		return db
	}
	everything := nostr.Filters{
		{Kinds: []int{events.KindApp}, Tags: nostr.TagMap{"d": []string{appID, solo}}, Limit: 100},
		{Kinds: []int{events.KindRelease, events.KindAsset}, Tags: nostr.TagMap{"i": []string{appID, solo}}, Limit: 100},
	}

	for name, tc := range map[string]struct {
		catalogCode string
		filters     nostr.Filters
		want        map[string]int
	}{
		"developer wins a tie":     {"2", everything, map[string]int{developer: 3, catalog: 3}},
		"higher version_code wins": {"3", everything, map[string]int{catalog: 6}},
		"authors keep their own listing": {"2", nostr.Filters{
			{Kinds: []int{events.KindApp, events.KindRelease, events.KindAsset}, Authors: []string{catalog}, Limit: 100},
		}, map[string]int{catalog: 6}},
	} {
		t.Run(name, func(t *testing.T) {
			db := setup(t, tc.catalogCode)
			if err := reselect(context.Background(), db, catalog); err != nil {
				t.Fatal(err)
			}
			kept := querySlots(t, db, tc.filters)
			got := map[string]int{}
			for _, event := range kept {
				got[event.PubKey]++
				if app, _ := slotAppID(event); app == solo && event.PubKey != catalog {
					t.Errorf("solo app event from %s", event.PubKey)
				}
			}
			total := 0
			for pubkey, n := range tc.want {
				total += n
				if got[pubkey] != n {
					t.Errorf("%s: %d events, want %d (all %v)", pubkey[:8], got[pubkey], n, got)
				}
			}
			if len(kept) != total {
				t.Errorf("kept %d events, want %d", len(kept), total)
			}
		})
	}
}

func TestReselect(t *testing.T) {
	const appID = "com.example.app"
	cert := strings.Repeat("cd", 32)
	catalog := testPubkey(t)
	developer := testPubkey(t)
	ctx := context.Background()
	filter := nostr.Filters{{Kinds: []int{events.KindApp}, Tags: nostr.TagMap{"d": []string{appID}}, Limit: 10}}
	signers := func(t *testing.T, db store.T) []string {
		t.Helper()
		var out []string
		for _, event := range querySlots(t, db, filter) {
			out = append(out, event.PubKey)
		}
		return out
	}
	stored := func(t *testing.T, db store.T) (string, bool) {
		t.Helper()
		var release sql.NullString
		err := db.DB.QueryRowContext(ctx, `SELECT release_id FROM listings WHERE app_id = ?`, appID).Scan(&release)
		if errors.Is(err, sql.ErrNoRows) {
			return "", false
		}
		if err != nil {
			t.Fatal(err)
		}
		return release.String, true
	}

	db := newSlotDB(t)
	saveListing(t, db, catalog, appID, cert, "1", "3")
	saveListing(t, db, developer, appID, cert, "2", "2")
	if _, ok := stored(t, db); ok {
		t.Fatal("a write clears the selection")
	}
	if err := reselect(ctx, db, catalog); err != nil {
		t.Fatal(err)
	}
	if release, _ := stored(t, db); release != strings.Repeat("11", 32) {
		t.Fatalf("selected release %q, want the catalog key's", release)
	}
	if got := signers(t, db); len(got) != 1 || got[0] != catalog {
		t.Fatalf("before: %v, want the catalog key", got)
	}
	if _, err := db.Delete(ctx, nostr.Filter{Authors: []string{catalog}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := stored(t, db); ok {
		t.Fatal("a deletion clears the selection")
	}
	if got := signers(t, db); len(got) != 1 || got[0] != developer {
		t.Fatalf("an app with no selection is not filtered: %v", got)
	}
	if err := reselect(ctx, db, catalog); err != nil {
		t.Fatal(err)
	}
	if release, _ := stored(t, db); release != strings.Repeat("21", 32) {
		t.Fatalf("selected release %q, want the developer's", release)
	}
}

func querySlots(t *testing.T, db store.T, filters nostr.Filters) []nostr.Event {
	t.Helper()
	ctx := context.Background()
	result, err := db.Query(ctx, filters...)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := selectSlots(ctx, db, filters, result)
	if err != nil {
		t.Fatal(err)
	}
	return kept
}

// saveListing stores a complete main-channel listing: app, release, and one arm64 asset.
func saveListing(t *testing.T, db store.T, pubkey, appID, cert, seed, versionCode string) {
	t.Helper()
	assetID := strings.Repeat(seed+"a", 32)
	saveSlotEvent(t, db, nostr.Event{
		ID:     strings.Repeat(seed+"0", 32),
		PubKey: pubkey,
		Kind:   events.KindApp,
		Tags:   nostr.Tags{{"d", appID}, {"name", "Example"}, {"f", "android-arm64-v8a"}},
	})
	saveSlotEvent(t, db, nostr.Event{
		ID:     strings.Repeat(seed+"1", 32),
		PubKey: pubkey,
		Kind:   events.KindRelease,
		Tags: nostr.Tags{
			{"i", appID},
			{"version", "1.0." + versionCode},
			{"d", fmt.Sprintf("%s@1.0.%s", appID, versionCode)},
			{"c", "main"},
			{"e", assetID},
		},
	})
	saveSlotEvent(t, db, nostr.Event{
		ID:     assetID,
		PubKey: pubkey,
		Kind:   events.KindAsset,
		Tags: nostr.Tags{
			{"i", appID},
			{"x", strings.Repeat(seed+"b", 32)},
			{"version", "1.0." + versionCode},
			{"m", "application/vnd.android.package-archive"},
			{"f", "android-arm64-v8a"},
			{"version_code", versionCode},
			{"apk_certificate_hash", cert},
		},
	})
}
