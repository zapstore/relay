package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pippellia-btc/blossom"
)

func TestKeepAPKRefusesOversize(t *testing.T) {
	prev := maxAPK
	maxAPK = 4
	t.Cleanup(func() { maxAPK = prev })

	dir := t.TempDir()
	blob := strings.Repeat("ab", 32)
	if _, _, err := keepAPK(strings.NewReader("abcde"), dir, blob); !errors.Is(err, errAPKTooBig) {
		t.Fatalf("keepAPK: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the partial download left %d files", len(entries))
	}
	if _, _, err := tempAPK(strings.NewReader("abcde"), blob); !errors.Is(err, errAPKTooBig) {
		t.Fatalf("tempAPK: %v", err)
	}
	if _, _, err := tempAPK(strings.NewReader("abcd"), blob); !errors.Is(err, errAPKHash) {
		t.Fatalf("a file at the limit was not read: %v", err)
	}
}

func TestOversizeDownloadIsRefusedNotRetried(t *testing.T) {
	prev := maxAPK
	maxAPK = 4
	t.Cleanup(func() { maxAPK = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("abcde"))
	}))
	t.Cleanup(srv.Close)

	blob := strings.Repeat("cd", 32)
	r := &T{
		apks:    newAPKReader(),
		blossom: emptyBlossom{},
		config:  Config{APKCacheDir: t.TempDir()},
		fetch:   srv.Client(),
	}
	if err := r.readAPKJob(t.Context(), apkJob{blob: blob, url: srv.URL}); err != nil {
		t.Fatalf("readAPKJob returned a retryable error: %v", err)
	}
	_, reason, ok := r.apks.cached(blob)
	if !ok || reason != errAPKTooBig.Error() {
		t.Fatalf("refusal %q, remembered %v", reason, ok)
	}
}

func TestOversizeBufferZoneAPKIsRefused(t *testing.T) {
	prev := maxAPK
	maxAPK = 4
	t.Cleanup(func() { maxAPK = prev })

	path := filepath.Join(t.TempDir(), "blob.apk")
	if err := os.WriteFile(path, []byte("abcde"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := strings.Repeat("ba", 32)
	r := &T{apks: newAPKReader(), blossom: fileBlossom{path: path}}
	if err := r.readAPKJob(t.Context(), apkJob{blob: blob}); err != nil {
		t.Fatalf("readAPKJob returned a retryable error: %v", err)
	}
	_, reason, ok := r.apks.cached(blob)
	if !ok || reason != errAPKTooBig.Error() {
		t.Fatalf("refusal %q, remembered %v", reason, ok)
	}
}

func TestFetchFailureIsNotRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	blob := strings.Repeat("ef", 32)
	r := &T{
		apks:    newAPKReader(),
		blossom: emptyBlossom{},
		fetch:   srv.Client(),
	}
	if err := r.readAPKJob(t.Context(), apkJob{blob: blob, url: srv.URL}); err == nil {
		t.Fatal("a failed fetch was not reported as an error")
	}
	if _, _, ok := r.apks.cached(blob); ok {
		t.Fatal("a failed fetch was remembered as a refusal")
	}
}

// fileBlossom holds one blob in the buffer zone, served as a seekable file.
type fileBlossom struct {
	emptyBlossom
	path string
}

func (fileBlossom) Has(context.Context, blossom.Hash) (bool, error) { return true, nil }

func (b fileBlossom) Open(context.Context, blossom.Hash) (io.ReadCloser, error) {
	return os.Open(b.path)
}

// emptyBlossom has no blobs, so every read falls through to the APK cache
// and then to the asset's url.
type emptyBlossom struct{}

func (emptyBlossom) Has(context.Context, blossom.Hash) (bool, error) { return false, nil }

func (emptyBlossom) Open(context.Context, blossom.Hash) (io.ReadCloser, error) {
	return nil, errors.New("no blob")
}

func (emptyBlossom) Promote(context.Context, blossom.Hash) error { return nil }

func (emptyBlossom) Expire(context.Context, time.Time) (int, error) { return 0, nil }
