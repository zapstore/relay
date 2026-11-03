package relay

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

// rejectedMax bounds the memory. The oldest entry goes first.
const rejectedMax = 4096

// rejection is why the relay refused one asset event.
type rejection struct {
	pubkey string
	appID  string
	reason string
	at     time.Time
}

// rejectedAssets remembers rejected kind 3063 events by id. A release that
// names one of them is not waiting for it: the asset will never arrive.
// An entry goes when a release that names it is promoted or refused. One that
// no release names goes after ttl, the time a pending event is kept. The state
// is in memory and is lost on restart; the publisher's next attempt records
// the rejection again. A nil value remembers nothing.
type rejectedAssets struct {
	mu   sync.Mutex
	byID map[string]rejection
	ttl  time.Duration
	now  func() time.Time
}

func newRejectedAssets(ttl time.Duration) *rejectedAssets {
	return &rejectedAssets{byID: map[string]rejection{}, ttl: ttl, now: time.Now}
}

// forget drops the rejected assets a release names, once it is resolved.
func (t *rejectedAssets) forget(release nostr.Event) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range events.FindAll(release.Tags, "e") {
		delete(t.byID, id)
	}
}

// add records the rejection of an asset event.
func (t *rejectedAssets) add(asset *nostr.Event, reason string) {
	if t == nil || asset.Kind != events.KindAsset {
		return
	}
	appID, _ := listingAppID(asset)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.expire(now)
	if len(t.byID) >= rejectedMax {
		t.dropOldest()
	}
	t.byID[asset.ID] = rejection{pubkey: asset.PubKey, appID: appID, reason: reason, at: now}
}

// assetRejected remembers a rejected asset and queues the promotion of the
// signer's pending listing of that app: a release that arrived first was
// waiting for it. It runs in the reject hook, on the connection's read loop,
// so it does no database work. A full queue drops the signal; the next
// release or the hourly sweep promotes the listing.
func (r *T) assetRejected(asset *nostr.Event, cause error) {
	r.rejected.add(asset, cause.Error())
	select {
	case r.rejections <- *asset:
	default:
	}
}

// promoteRejected promotes the pending listing of a rejected asset's signer.
func (r *T) promoteRejected(ctx context.Context, asset nostr.Event) {
	appID, ok := listingAppID(&asset)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := r.promoteListing(ctx, asset.PubKey, appID, ""); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("relay: failed to promote after a rejected asset", "event", asset.ID, "error", err)
	}
}

// get returns the rejection of asset id, if it is still remembered.
func (t *rejectedAssets) get(id string) (rejection, bool) {
	if t == nil {
		return rejection{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	got, ok := t.byID[id]
	if ok && t.now().Sub(got.at) > t.ttl {
		delete(t.byID, id)
		return rejection{}, false
	}
	return got, ok
}

// expire deletes the entries older than ttl. The caller holds mu.
func (t *rejectedAssets) expire(now time.Time) {
	for id, got := range t.byID {
		if now.Sub(got.at) > t.ttl {
			delete(t.byID, id)
		}
	}
}

// dropOldest deletes the oldest entry. The caller holds mu.
func (t *rejectedAssets) dropOldest() {
	var oldest string
	var at time.Time
	for id, got := range t.byID {
		if oldest == "" || got.at.Before(at) {
			oldest, at = id, got.at
		}
	}
	delete(t.byID, oldest)
}
