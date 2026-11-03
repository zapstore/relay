package relay

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/blossom"
	"github.com/zapstore/relay/pkg/relay/store"
)

// desktopAsset is an asset the relay rejects for its platform.
func desktopAsset(pub string) *nostr.Event {
	asset := testAsset("b", pub, strings.Repeat("d", 64), strings.Repeat("e", 64), 1)
	for i, tag := range asset.Tags {
		if tag[0] == "f" {
			asset.Tags[i] = nostr.Tag{"f", "linux-x86_64"}
		}
	}
	return asset
}

// rejectedTriple is a catalog listing whose release names an APK and a
// desktop asset the relay rejected.
func rejectedTriple(t *testing.T) (r *T, app, release, apk, desktop *nostr.Event) {
	t.Helper()
	blobs := newFakeBlossom()
	r = testRelay(t, blobs)
	r.rejected = newRejectedAssets(time.Hour)
	app, release, apk = testTriple("a", catalogKey, blobs.put("apk"), strings.Repeat("e", 64), 1)
	desktop = desktopAsset(catalogKey)
	release.Tags = append(release.Tags, nostr.Tag{"e", desktop.ID})
	return r, app, release, apk, desktop
}

func TestReleaseSkipsRejectedAssets(t *testing.T) {
	r, app, release, apk, desktop := rejectedTriple(t)
	r.rejected.add(desktop, "platform must include android-arm64-v8a")

	r.holdListing(t.Context(), app)
	r.holdListing(t.Context(), apk)
	res := r.holdListing(t.Context(), release)
	if !res.ok || !res.broadcast || !slotHas(t, r.store, release.ID) {
		t.Fatalf("a release with a rejected asset was not listed: %+v", res)
	}
	if !strings.Contains(res.msg, "1 of 2 assets were rejected") || !strings.Contains(res.msg, "platform must include") {
		t.Fatalf("reply does not say what was skipped: %q", res.msg)
	}
	if _, ok := r.rejected.get(desktop.ID); ok {
		t.Fatal("the rejection was kept after the release was promoted")
	}
}

func TestReleaseWaitsForAnAssetNotRejected(t *testing.T) {
	r, app, release, apk, _ := rejectedTriple(t)

	r.holdListing(t.Context(), app)
	r.holdListing(t.Context(), apk)
	res := r.holdListing(t.Context(), release)
	if !res.ok || res.broadcast || slotHas(t, r.store, release.ID) {
		t.Fatalf("a release missing an unknown asset was promoted: %+v", res)
	}
	if !strings.HasPrefix(res.msg, msgListingPending) || !strings.Contains(res.msg, "waiting for 1 of 2 assets") {
		t.Fatalf("reply does not say what it waits for: %q", res.msg)
	}
}

func TestReleaseRefusedWhenEveryAssetWasRejected(t *testing.T) {
	r, app, release, _, desktop := rejectedTriple(t)
	release.Tags = nostr.Tags{
		{"a", "32267:" + catalogKey + ":" + testApp}, {"i", testApp}, {"version", "1.0.0"},
		{"d", testApp + "@1.0.0"}, {"c", "main"}, {"e", desktop.ID}, {"f", "android-arm64-v8a"},
	}
	r.rejected.add(desktop, "platform must include android-arm64-v8a")

	r.holdListing(t.Context(), app)
	res := r.holdListing(t.Context(), release)
	if res.ok || !strings.Contains(res.msg, "no asset of the release was accepted") {
		t.Fatalf("a release with no accepted asset: %+v", res)
	}
	if slotHas(t, r.store, release.ID) {
		t.Fatal("a release with no accepted asset was stored")
	}
	if _, ok := r.rejected.get(desktop.ID); ok {
		t.Fatal("the rejection was kept after the release was refused")
	}
}

func TestRejectedAssetPromotesAReleaseThatArrivedFirst(t *testing.T) {
	r, app, release, apk, desktop := rejectedTriple(t)

	r.holdListing(t.Context(), app)
	r.holdListing(t.Context(), apk)
	r.holdListing(t.Context(), release)
	if slotHas(t, r.store, release.ID) {
		t.Fatal("the release was promoted before the asset was rejected")
	}
	r.rejections = make(chan nostr.Event, 1)
	r.assetRejected(desktop, errors.New("platform must include android-arm64-v8a"))
	select {
	case asset := <-r.rejections:
		r.promoteRejected(t.Context(), asset)
	default:
		t.Fatal("the rejection was not queued")
	}
	if !slotHas(t, r.store, release.ID) {
		t.Fatal("the rejection did not promote the release")
	}
	if _, ok := r.rejected.get(desktop.ID); ok {
		t.Fatal("the rejection was kept after the release was promoted")
	}
}

func TestRejectionIsKeptWhileTheReleaseWaits(t *testing.T) {
	r, app, release, _, desktop := rejectedTriple(t)
	r.rejected.add(desktop, "platform must include android-arm64-v8a")

	// The APK has not arrived, so the release is still pending.
	r.holdListing(t.Context(), app)
	r.holdListing(t.Context(), release)
	if slotHas(t, r.store, release.ID) {
		t.Fatal("the release was promoted without its APK")
	}
	if _, ok := r.rejected.get(desktop.ID); !ok {
		t.Fatal("the rejection was forgotten while the release waits")
	}
}

func TestRejectedAssetIsForTheSameSigner(t *testing.T) {
	r, app, release, apk, desktop := rejectedTriple(t)
	other := *desktop
	other.PubKey = strings.Repeat("9", 64)
	r.rejected.add(&other, "platform must include android-arm64-v8a")

	r.holdListing(t.Context(), app)
	r.holdListing(t.Context(), apk)
	if res := r.holdListing(t.Context(), release); res.broadcast || slotHas(t, r.store, release.ID) {
		t.Fatalf("another signer's rejection let the release through: %+v", res)
	}
}

func TestRejectedAssetsExpire(t *testing.T) {
	now := time.Now()
	tracker := newRejectedAssets(time.Hour)
	tracker.now = func() time.Time { return now }
	asset := desktopAsset(catalogKey)
	tracker.add(asset, "reason")
	if _, ok := tracker.get(asset.ID); !ok {
		t.Fatal("a fresh rejection was forgotten")
	}
	now = now.Add(time.Hour + time.Second)
	if _, ok := tracker.get(asset.ID); ok {
		t.Fatal("an old rejection was kept")
	}
}

func TestRejectedAssetsAreBounded(t *testing.T) {
	now := time.Now()
	tracker := newRejectedAssets(time.Hour)
	tracker.now = func() time.Time { now = now.Add(time.Millisecond); return now }
	first := desktopAsset(catalogKey)
	tracker.add(first, "reason")
	for i := 0; i < rejectedMax; i++ {
		asset := desktopAsset(catalogKey)
		asset.ID = strings.Repeat("f", 60) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676%26)) + "0"
		tracker.add(asset, "reason")
	}
	if len(tracker.byID) > rejectedMax {
		t.Fatalf("kept %d entries, want at most %d", len(tracker.byID), rejectedMax)
	}
	if _, ok := tracker.get(first.ID); ok {
		t.Fatal("the oldest entry was kept over a newer one")
	}
}

func TestNilRejectedAssets(t *testing.T) {
	var tracker *rejectedAssets
	tracker.add(desktopAsset(catalogKey), "reason")
	if _, ok := tracker.get("x"); ok {
		t.Fatal("a nil tracker remembered an asset")
	}
}

// TestConcurrentPublishOfOneListing sends the events of one listing from
// parallel workers, as the relay's processor does, and a rejection with them.
// Whatever the order, the release is stored once and nothing stays pending.
// lockedBlossom lets parallel promotions share the fake, whose slices are not safe.
type lockedBlossom struct {
	mu sync.Mutex
	*fakeBlossom
}

func (b *lockedBlossom) Has(ctx context.Context, hash blossom.Hash) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fakeBlossom.Has(ctx, hash)
}

func (b *lockedBlossom) Promote(ctx context.Context, hash blossom.Hash) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fakeBlossom.Promote(ctx, hash)
}

func TestConcurrentPublishOfOneListing(t *testing.T) {
	for round := 0; round < 20; round++ {
		r, app, release, apk, desktop := rejectedTriple(t)
		// :memory: is one database per connection, so parallel workers need a file.
		db, err := store.New(filepath.Join(t.TempDir(), "relay.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		r.store = db
		r.blossom = &lockedBlossom{fakeBlossom: r.blossom.(*fakeBlossom)}
		r.rejections = make(chan nostr.Event, 8)
		var wg sync.WaitGroup
		var failed atomic.Int32
		for _, event := range []*nostr.Event{release, apk, app} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if res := r.holdListing(t.Context(), event); res.msg == ErrInternal.Error() {
					failed.Add(1)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.assetRejected(desktop, errors.New("platform must include android-arm64-v8a"))
			select {
			case asset := <-r.rejections:
				r.promoteRejected(t.Context(), asset)
			default:
			}
		}()
		wg.Wait()
		if failed.Load() != 0 {
			t.Fatalf("round %d: %d events were answered with an internal error", round, failed.Load())
		}

		// Late arrivals settle what the parallel run left waiting.
		r.promoteRejected(t.Context(), *desktop)
		if !slotHas(t, r.store, release.ID) {
			t.Fatalf("round %d: the release was not stored", round)
		}
		if kept, err := pendingHas(t.Context(), r.store, release.ID); err != nil || kept {
			t.Fatalf("round %d: the release is still pending (%v)", round, err)
		}
		var n int
		err = r.store.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM events WHERE id = ?`, release.ID).Scan(&n)
		if err != nil || n != 1 {
			t.Fatalf("round %d: release stored %d times (%v)", round, n, err)
		}
	}
}
