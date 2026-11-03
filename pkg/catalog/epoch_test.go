package catalog

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestFromWithoutSnapshotGetsFullSnapshot(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(SnapDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SnapPath(dir, 5), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(BundleDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(BundlePath(dir, 0, 5), []byte("full"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := Handler{Data: dir}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/bundle?from=2", nil)
	req.RemoteAddr = "192.0.2.1:9"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "full" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
}
