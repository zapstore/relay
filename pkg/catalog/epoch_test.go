package catalog

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestRebuildContinuesAboveServedEpochs(t *testing.T) {
	data := t.TempDir()
	signer, err := ParseSigner("1")
	if err != nil {
		t.Fatal(err)
	}
	first := State{Listings: []Listing{apkListing("com.example.one", "")}}
	second := State{Listings: []Listing{apkListing("com.example.one", ""), apkListing("com.example.two", "")}}
	if n, err := CreateBundle(t.Context(), data, first, signer, 10, nil); err != nil || n != 1 {
		t.Fatalf("first epoch %d, %v", n, err)
	}
	if n, err := CreateBundle(t.Context(), data, second, signer, 20, nil); err != nil || n != 2 {
		t.Fatalf("second epoch %d, %v", n, err)
	}
	if err := os.RemoveAll(SnapDir(data)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(BundleDir(data)); err != nil {
		t.Fatal(err)
	}
	n, err := CreateBundle(t.Context(), data, first, signer, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("rebuilt epoch = %d, want 3", n)
	}
	if _, err := os.Stat(BundlePath(data, 0, 3)); err != nil {
		t.Fatalf("full bundle: %v", err)
	}
	if floor, err := ReadFloor(data); err != nil || floor != 3 {
		t.Fatalf("floor %d, %v", floor, err)
	}
}

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
