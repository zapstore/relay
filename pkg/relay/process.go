package relay

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/catalog"
	"github.com/zapstore/relay/pkg/events"
)

// Processor runs on a release that passed admission, just before it is
// listed: steroid's enrichment, and later security checks. A refusal drops
// the release with that reason. An error retries it after processRetry.
type Processor func(ctx context.Context, listing catalog.Listing, picture string) (refusal string, err error)

const (
	processRetry     = 10 * time.Minute
	processTimeout   = 15 * time.Minute
	processQueueSize = 64
)

// processQueue runs the Processor one release at a time, off the event path.
// Results are kept by release id until the process exits.
type processQueue struct {
	jobs chan catalog.Listing

	mu      sync.Mutex
	queued  map[string]struct{}
	retryAt map[string]time.Time
	done    map[string]string // release id: refusal, "" when it passed
}

func newProcessQueue() *processQueue {
	return &processQueue{
		jobs:    make(chan catalog.Listing, processQueueSize),
		queued:  map[string]struct{}{},
		retryAt: map[string]time.Time{},
		done:    map[string]string{},
	}
}

func (q *processQueue) result(release string) (refusal string, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	refusal, ok = q.done[release]
	return refusal, ok
}

// request queues a listing unless it is queued or waiting to retry.
func (q *processQueue) request(listing catalog.Listing) {
	q.mu.Lock()
	defer q.mu.Unlock()
	id := listing.Release.ID
	if _, ok := q.queued[id]; ok {
		return
	}
	if time.Now().Before(q.retryAt[id]) {
		return
	}
	select {
	case q.jobs <- listing:
		q.queued[id] = struct{}{}
	default:
		slog.Warn("process queue is full", "release", id)
	}
}

func (q *processQueue) finish(release, refusal string, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.queued, release)
	if err != nil {
		q.retryAt[release] = time.Now().Add(processRetry)
		return
	}
	delete(q.retryAt, release)
	q.done[release] = refusal
}

// UseProcessor sets the step every admitted release passes before it is
// listed. Without one, an admitted release is listed at once.
func (r *T) UseProcessor(p Processor) {
	r.process = p
	if r.procs == nil {
		r.procs = newProcessQueue()
	}
}

// processed reports whether an admitted release went through the Processor,
// queueing it when it has not. A release whose every APK is already listed
// was processed when it was first listed, so a key rotation is not queued.
func (r *T) processed(ctx context.Context, appID string, app *nostr.Event, release nostr.Event, assets []nostr.Event) (refusal string, ok bool, err error) {
	if r.process == nil {
		return "", true, nil
	}
	if refusal, ok := r.procs.result(release.ID); ok {
		return refusal, true, nil
	}
	listed := true
	for _, asset := range assets {
		x, _ := events.Find(asset.Tags, "x")
		has, err := r.listedFile(ctx, x)
		if err != nil {
			return "", false, err
		}
		if !has {
			listed = false
			break
		}
	}
	if listed {
		return "", true, nil
	}
	r.procs.request(catalog.Listing{AppID: appID, App: *app, Release: release, Assets: assets})
	return "", false, nil
}

func (r *T) runProcessor(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case listing := <-r.procs.jobs:
			refusal, err := r.processJob(ctx, listing)
			r.procs.finish(listing.Release.ID, refusal, err)
			if err != nil {
				slog.Warn("processor: failed", "release", listing.Release.ID, "app", listing.AppID, "error", err)
				continue
			}
			r.promoteProcessed(ctx, listing)
		}
	}
}

// promoteProcessed promotes the app of a processed release, without waiting
// for the next reconcile.
func (r *T) promoteProcessed(ctx context.Context, listing catalog.Listing) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := r.promoteListing(ctx, listing.Release.PubKey, listing.AppID, ""); err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Error("processor: promote failed", "release", listing.Release.ID, "app", listing.AppID, "error", err)
		}
		return
	}
	r.reselect(ctx)
}

func (r *T) processJob(ctx context.Context, listing catalog.Listing) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, processTimeout)
	defer cancel()
	profile, _, err := loadEvent(ctx, r.store, `
		SELECT id, pubkey, created_at, kind, tags, content, sig FROM events
		WHERE kind = 0 AND pubkey = ? ORDER BY created_at DESC LIMIT 1
	`, listing.App.PubKey)
	if err != nil {
		return "", err
	}
	return r.process(ctx, listing, catalog.ProfilePicture(profile))
}
