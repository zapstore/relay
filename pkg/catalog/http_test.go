package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/rate"
)

func TestBundleRateLimitAppliesToBuild(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(SnapDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SnapPath(dir, 1), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := Handler{Limit: drainedLimiter(), Data: dir}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bundleReq())
	if rec.Code != http.StatusTooManyRequests || rec.Body.Len() != 0 {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After %q, want 60", got)
	}
}

func TestBundleNotModifiedIsFree(t *testing.T) {
	h := Handler{Limit: drainedLimiter(), Data: t.TempDir()}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bundleReq())
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestBundleCachedIsFree(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(SnapDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SnapPath(dir, 1), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(BundleDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(BundlePath(dir, 0, 1), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := Handler{Limit: drainedLimiter(), Data: dir}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bundleReq())
	if rec.Code != http.StatusOK || rec.Body.String() != "cached" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
}

func drainedLimiter() rate.Limiter {
	lim := rate.NewLimiter(rate.NewConfig())
	for lim.Allow("192.0.2.1", 1) {
	}
	return lim
}

func bundleReq() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/v1/bundle?from=0", nil)
	req.RemoteAddr = "192.0.2.1:9"
	return req
}

func TestBundleSharedBuild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(SnapDir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(SnapPath(dir, 1), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		release := make(chan struct{})
		var calls atomic.Int32
		h := Handler{
			Data: dir,
			build: func(_ context.Context, from, to int64) error {
				calls.Add(1)
				<-release
				return writeAtomic(BundlePath(dir, from, to), []byte("bundle"))
			},
		}.Shared()
		var wg sync.WaitGroup
		wg.Add(2)
		for range 2 {
			go func() {
				defer wg.Done()
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bundle?from=0", nil))
				if rec.Code != http.StatusOK || rec.Body.String() != "bundle" {
					t.Errorf("status %d body %q", rec.Code, rec.Body.String())
				}
			}()
		}
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("builds = %d, want 1", calls.Load())
		}
		close(release)
		wg.Wait()
	})
}

func TestBundleBuildsBeyondCapacityAreRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(SnapDir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		const builds = 2
		for n := int64(1); n <= builds+1; n++ {
			if err := os.WriteFile(SnapPath(dir, n), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		release := make(chan struct{})
		h := Handler{
			Data:   dir,
			Builds: builds,
			build: func(_ context.Context, from, to int64) error {
				<-release
				return writeAtomic(BundlePath(dir, from, to), []byte("bundle"))
			},
		}.Shared()
		var wg sync.WaitGroup
		for from := range builds {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bundle?from="+strconv.Itoa(from), nil))
				if rec.Code != http.StatusOK {
					t.Errorf("from %d status %d", from, rec.Code)
				}
			}()
		}
		synctest.Wait()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bundle?from="+strconv.Itoa(builds), nil))
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("status %d retry-after %q", rec.Code, rec.Header().Get("Retry-After"))
		}
		close(release)
		wg.Wait()
	})
}

func TestPruneBundlesKeepsCurrentAndTemp(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(BundleDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0-1.tar.zst", "1-2.tar.zst", "1-2.full", "0-3.tar.zst", "1-3.tar.zst", "1-3.full", "2-3.tar.zst", "2-3.full", ".partial-9"} {
		if err := os.WriteFile(filepath.Join(BundleDir(dir), name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneBundles(dir, 3, 2); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(BundleDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{".partial-9", "0-3.tar.zst", "2-3.full", "2-3.tar.zst"}; !slices.Equal(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
}

type recordedHit struct {
	from, to int64
	status   int
	protocol string
	client   string
}

type hitLog struct {
	hits []recordedHit
}

func (h *hitLog) RecordBundle(from, to int64, status int, protocol, client string) {
	h.hits = append(h.hits, recordedHit{from, to, status, protocol, client})
}

func TestBundleRecordsResponse(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(SnapDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SnapPath(dir, 2), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := &hitLog{}
	h := Handler{Data: dir, Count: log}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bundle?from=2&v=2.0.1", nil))
	if rec.Code != http.StatusNotModified {
		t.Fatalf("304 status %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/bundle?from=2&v=9.9.9", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("v2 status %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bundle?from=2&v=not-a-version", nil))
	if rec.Code != http.StatusNotModified {
		t.Fatalf("bad client status %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bundle?from=2", nil))
	if rec.Code != http.StatusNotModified {
		t.Fatalf("missing client status %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/bundle?from=2&v=2.0.1", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post status %d", rec.Code)
	}

	want := []recordedHit{
		{2, 2, http.StatusNotModified, "v1", "2.0.1"},
		{2, unknownEpoch, http.StatusBadRequest, "v2", "9.9.9"},
		{2, 2, http.StatusNotModified, "v1", ""},
		{2, 2, http.StatusNotModified, "v1", ""},
		{2, unknownEpoch, http.StatusMethodNotAllowed, "v1", "2.0.1"},
	}
	if len(log.hits) != len(want) {
		t.Fatalf("hits %d, want %d: %+v", len(log.hits), len(want), log.hits)
	}
	for i, hit := range log.hits {
		if hit != want[i] {
			t.Errorf("hit %d = %+v, want %+v", i, hit, want[i])
		}
	}
}

func TestBundleLargeDeltaServesFull(t *testing.T) {
	data := t.TempDir()
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"com.example.a", "com.example.b", "com.example.c", "com.example.d"}
	var raw []nostr.Event
	for _, id := range ids {
		raw = append(raw, appEvents(id)...)
	}
	seal := func(evs []nostr.Event, at int64) {
		t.Helper()
		if _, err := CreateBundle(t.Context(), data, Resolve(evs, ""), signer, at, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	seal(raw, 10)
	// Three of four apps gain files, so 1→3 ships 3 of 5 apps.
	for _, id := range ids[:3] {
		writeAbout(t, data, id)
		writeCache(t, data, id, appHash(id))
	}
	seal(raw, 20)
	seal(append(slices.Clone(raw), appEvents("com.example.e")...), 30)

	h := Handler{Data: data, Signer: signer, MaxShare: 0.5}.Shared()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bundle?from=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	full, err := os.ReadFile(BundlePath(data, 0, 3))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.Body.Bytes(), full) {
		t.Fatal("from=1 did not get the 0→3 bundle")
	}
	if _, err := os.Stat(BundlePath(data, 1, 3)); !os.IsNotExist(err) {
		t.Fatalf("1→3 was written: %v", err)
	}

	h.Limit = drainedLimiter()
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/bundle?from=1", nil)
	req.RemoteAddr = "192.0.2.1:9"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !slices.Equal(rec.Body.Bytes(), full) {
		t.Fatalf("repeat status %d, want the cached 0→3 bundle for free", rec.Code)
	}
}

func TestBundleSmallDeltaIsKept(t *testing.T) {
	data := t.TempDir()
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"com.example.a", "com.example.b", "com.example.c", "com.example.d"}
	for _, id := range ids {
		writeAbout(t, data, id)
		writeCache(t, data, id, appHash(id))
	}
	var raw []nostr.Event
	for _, id := range ids[:3] {
		raw = append(raw, appEvents(id)...)
	}
	if _, err := CreateBundle(t.Context(), data, Resolve(raw, ""), signer, 10, nil, 0); err != nil {
		t.Fatal(err)
	}
	// 1→3 ships one app's files of five apps: one new app with files, one without.
	raw = append(raw, appEvents(ids[3])...)
	if _, err := CreateBundle(t.Context(), data, Resolve(raw, ""), signer, 20, nil, 0); err != nil {
		t.Fatal(err)
	}
	raw = append(raw, appEvents("com.example.e")...)
	if _, err := CreateBundle(t.Context(), data, Resolve(raw, ""), signer, 30, nil, 0); err != nil {
		t.Fatal(err)
	}
	if n, err := LatestSnap(data); err != nil || n != 3 {
		t.Fatalf("latest %d %v, want 3", n, err)
	}
	if err := WriteBundle(t.Context(), data, 1, 3, 0.5, signer, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(BundlePath(data, 1, 3)); err != nil {
		t.Fatal(err)
	}
}

func TestBundleLostSnapshotServesFull(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(SnapDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	for n := int64(1); n <= 2; n++ {
		if err := os.WriteFile(SnapPath(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := Handler{
		Data: dir,
		build: func(_ context.Context, from, to int64) error {
			if from > 0 {
				// A seal pruned snapshot from while this build read it.
				return ErrFullBundle
			}
			return writeAtomic(BundlePath(dir, from, to), []byte("full"))
		},
	}.Shared()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bundle?from=1", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "full" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestSealPrunesSnapshotsBeyondKeep(t *testing.T) {
	data := t.TempDir()
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	const keep = 2
	for i := range 5 {
		st := State{Listings: []Listing{apkListing(fmt.Sprintf("com.example.app%d", i), "")}}
		if _, err := CreateBundle(t.Context(), data, st, signer, int64(10*(i+1)), nil, keep); err != nil {
			t.Fatal(err)
		}
	}
	for n := int64(1); n <= 5; n++ {
		_, err := os.Stat(SnapPath(data, n))
		if kept := n >= 5-keep; kept != (err == nil) {
			t.Errorf("snapshot %d: kept %t, stat %v", n, kept, err)
		}
	}
	entries, err := os.ReadDir(BundleDir(data))
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{"4-5.tar.zst"}; !slices.Equal(left, want) {
		t.Fatalf("bundles %v, want %v", left, want)
	}

	h := Handler{Data: data, Signer: signer, MaxShare: 0.5}.Shared()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bundle?from=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pruned from status %d body %q", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(BundlePath(data, 0, 5)); err != nil {
		t.Fatalf("pruned from did not build 0→5: %v", err)
	}
}

func appHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// appEvents is an app, release, and APK asset that resolve to one listing.
func appEvents(id string) []nostr.Event {
	pub := strings.Repeat("ab", 32)
	hash := appHash(id)
	return []nostr.Event{
		{
			ID: id + "-app", Kind: events.KindApp, PubKey: pub,
			Tags: nostr.Tags{{"d", id}, {"name", id}, {"f", "android-arm64-v8a"}},
		},
		{
			ID: id + "-rel", Kind: events.KindRelease, PubKey: pub,
			Tags: nostr.Tags{
				{"i", id}, {"version", "1.0.0"}, {"d", id + "@1.0.0"},
				{"c", "main"}, {"e", hash},
			},
		},
		{
			ID: hash, Kind: events.KindAsset, PubKey: pub,
			Tags: nostr.Tags{
				{"i", id}, {"x", hash}, {"version", "1.0.0"},
				{"f", "android-arm64-v8a"}, {"version_code", "1"},
				{"apk_certificate_hash", hash},
				{"url", "https://example.com/" + id + ".apk"},
			},
		},
	}
}
