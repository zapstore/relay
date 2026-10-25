package relay

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zapstore/relay/pkg/catalog"
)

func TestAdmittedListingIsProcessedBeforeItIsListed(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	key := newTestKey(t)
	r.readAPK = readChains(t, map[string]signing{"apk": {key.cert}})
	var got []catalog.Listing
	r.UseProcessor(func(_ context.Context, listing catalog.Listing, _ string) (string, error) {
		if slotHas(t, r.store, listing.Release.ID) {
			t.Error("processed after it was listed")
		}
		got = append(got, listing)
		return "", nil
	})
	app, release, asset := testTriple("a", catalogKey, blobs.put("apk"), key.hash, 1)
	r.holdListing(t.Context(), app)
	r.holdListing(t.Context(), asset)
	r.holdListing(t.Context(), release)

	drainReads(t, r)
	if slotHas(t, r.store, release.ID) {
		t.Fatal("listed before it was processed")
	}
	drainProcessing(t, r)
	if !slotHas(t, r.store, release.ID) {
		t.Fatal("processed listing was not listed")
	}
	if len(got) != 1 || got[0].AppID != testApp || got[0].Release.ID != release.ID || len(got[0].Assets) != 1 {
		t.Fatalf("processed %+v", got)
	}
}

func TestProcessorRefusalDropsTheRelease(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	key := newTestKey(t)
	r.readAPK = readChains(t, map[string]signing{"apk": {key.cert}})
	r.UseProcessor(func(context.Context, catalog.Listing, string) (string, error) {
		return "the APK fails a security check", nil
	})
	app, release, asset := testTriple("a", catalogKey, blobs.put("apk"), key.hash, 1)
	r.holdListing(t.Context(), app)
	r.holdListing(t.Context(), asset)
	r.holdListing(t.Context(), release)
	drainReads(t, r)
	drainProcessing(t, r)
	if slotHas(t, r.store, release.ID) {
		t.Fatal("a refused release was listed")
	}
	if kept, _ := pendingHas(t.Context(), r.store, release.ID); kept {
		t.Fatal("a refused release is still pending")
	}
}

func TestProcessorErrorStillLists(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	key := newTestKey(t)
	r.readAPK = readChains(t, map[string]signing{"apk": {key.cert}})
	r.UseProcessor(func(context.Context, catalog.Listing, string) (string, error) {
		return "", errors.New("model unavailable")
	})
	app, release, asset := testTriple("a", catalogKey, blobs.put("apk"), key.hash, 1)
	r.holdListing(t.Context(), app)
	r.holdListing(t.Context(), asset)
	r.holdListing(t.Context(), release)
	drainReads(t, r)
	drainProcessing(t, r)
	if !slotHas(t, r.store, release.ID) {
		t.Fatal("a release whose processing failed was not listed")
	}
}

func TestListedFileIsNotProcessedAgain(t *testing.T) {
	blobs := newFakeBlossom()
	r := testRelay(t, blobs)
	r.UseProcessor(func(context.Context, catalog.Listing, string) (string, error) {
		t.Error("an already listed file was processed")
		return "", nil
	})
	x := blobs.put("apk")
	prior := testAsset("p", strings.Repeat("9", 64), x, strings.Repeat("e", 64), 1)
	saveSlotEvent(t, r.store, *prior)
	app, release, asset := testTriple("a", catalogKey, x, strings.Repeat("e", 64), 1)
	r.holdListing(t.Context(), app)
	r.holdListing(t.Context(), asset)
	if res := r.holdListing(t.Context(), release); !res.broadcast {
		t.Fatalf("release: %+v", res)
	}
}

// drainProcessing runs queued processing and promotes each processed app
// until nothing is queued.
func drainProcessing(t *testing.T, r *T) {
	t.Helper()
	for len(r.procs.jobs) > 0 {
		listing := <-r.procs.jobs
		refusal, _ := r.processJob(t.Context(), listing)
		r.procs.finish(listing.Release.ID, refusal)
		r.promoteProcessed(t.Context(), listing)
	}
}
