package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

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
			build: func(context.Context, int64, int64) ([]byte, error) {
				calls.Add(1)
				<-release
				return []byte("bundle"), nil
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
