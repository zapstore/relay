package blossom

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pippellia-btc/blossom"
	"github.com/pippellia-btc/blossy"
	"github.com/zapstore/relay/pkg/blossom/bunny"
	"github.com/zapstore/relay/pkg/blossom/store"
)

func TestUploadWaitsInBufferZone(t *testing.T) {
	b, server := bufferServer(t, nil)
	hash := uploadBlob(t, b, []byte("listing icon"))

	if code := get(server, hash).Code; code != http.StatusNotFound {
		t.Fatalf("pending GET = %d, want 404", code)
	}
	if code := head(server, hash); code != http.StatusNotFound {
		t.Fatalf("pending HEAD = %d, want 404", code)
	}

	if err := b.blobs.Promote(t.Context(), hash); err != nil {
		t.Fatal(err)
	}
	res := get(server, hash)
	if res.Code != http.StatusOK || res.Body.String() != "listing icon" {
		t.Fatalf("promoted GET = %d %q", res.Code, res.Body.String())
	}
}

func TestPromotedBlobMovesToBunny(t *testing.T) {
	cdn := &fakeCDN{files: map[string][]byte{}}
	b, server := bufferServer(t, cdn)
	hash := uploadBlob(t, b, []byte("listing icon"))
	if len(cdn.files) != 0 {
		t.Fatal("an upload reached Bunny before promotion")
	}

	if err := b.blobs.Promote(t.Context(), hash); err != nil {
		t.Fatal(err)
	}
	if res := get(server, hash); res.Code != http.StatusOK {
		t.Fatalf("moving GET = %d, want 200 from disk", res.Code)
	}

	if err := b.blobs.Move(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := BlobPath(hash, "image/png")
	if !bytes.Equal(cdn.files[path], []byte("listing icon")) {
		t.Fatalf("bunny has %q", cdn.files[path])
	}
	if _, err := os.Stat(filepath.Join(b.blobs.Files.Dir, path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disk copy remains: %v", err)
	}
	res := get(server, hash)
	if res.Code != http.StatusTemporaryRedirect || res.Header().Get("Location") != "https://cdn.test/"+path {
		t.Fatalf("served GET = %d %q", res.Code, res.Header().Get("Location"))
	}
}

func TestExpireAndPruneSplitTheBufferZone(t *testing.T) {
	b, _ := bufferServer(t, nil)
	pending := uploadBlob(t, b, []byte("pending"))
	served := uploadBlob(t, b, []byte("served"))
	if err := b.blobs.Promote(t.Context(), served); err != nil {
		t.Fatal(err)
	}

	if n, err := b.Prune(t.Context(), map[string]struct{}{}); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if has(t, b, served) || !has(t, b, pending) {
		t.Fatal("prune must delete the served blob and leave the pending one")
	}

	if n, err := b.blobs.Expire(t.Context(), time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("early expire: %d %v", n, err)
	}
	if n, err := b.blobs.Expire(t.Context(), time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	if has(t, b, pending) {
		t.Fatal("expired blob remains")
	}
	if _, err := os.Stat(filepath.Join(b.blobs.Files.Dir, BlobPath(pending, "image/png"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired file remains: %v", err)
	}
}

func TestOldBlobsAreServed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blossom.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE blobs (hash TEXT PRIMARY KEY, type TEXT NOT NULL, size INTEGER NOT NULL, created_at INTEGER NOT NULL, auth_pubkey TEXT);
		INSERT INTO blobs VALUES ('` + blossom.ComputeHash([]byte("old")).Hex() + `', 'image/png', 3, 1, NULL);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	meta, err := s.Query(t.Context(), blossom.ComputeHash([]byte("old")))
	if err != nil || meta.State != store.Served {
		t.Fatalf("meta %+v err %v", meta, err)
	}
}

func bufferServer(t *testing.T, cdn *fakeCDN) (*T, *blossy.Server) {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "blossom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server, err := blossy.NewServer(blossy.WithHostname("localhost"))
	if err != nil {
		t.Fatal(err)
	}
	blobs := Bytes{DB: db, Files: Files{Dir: t.TempDir()}, Local: cdn == nil}
	if cdn != nil {
		blobs.Bunny = cdn
	}
	b := &T{
		server: server,
		config: Config{Dir: blobs.Files.Dir, StallTimeout: 30 * time.Second},
		blobs:  blobs,
		store:  db,
		relay:  stubRelay{},
	}
	server.On.Check = b.check
	server.On.Download = b.download
	return b, server
}

func uploadBlob(t *testing.T, b *T, payload []byte) blossom.Hash {
	t.Helper()
	hash := blossom.ComputeHash(payload)
	req := httptest.NewRequest(http.MethodPut, "http://localhost/upload", bytes.NewReader(payload))
	hints := blossy.UploadHints{Hash: &hash, Type: "image/png", Size: int64(len(payload))}
	if _, err := b.upload(fakeRequest{raw: req, pubkey: "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"}, hints, bytes.NewReader(payload)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	return hash
}

func get(server *blossy.Server, hash blossom.Hash) *httptest.ResponseRecorder {
	res := httptest.NewRecorder()
	server.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "http://localhost/"+hash.Hex()+".png", nil))
	return res
}

func head(server *blossy.Server, hash blossom.Hash) int {
	res := httptest.NewRecorder()
	server.ServeHTTP(res, httptest.NewRequest(http.MethodHead, "http://localhost/"+hash.Hex()+".png", nil))
	return res.Code
}

func has(t *testing.T, b *T, hash blossom.Hash) bool {
	t.Helper()
	ok, err := b.blobs.Has(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

type fakeCDN struct {
	files map[string][]byte
}

func (f *fakeCDN) Upload(_ context.Context, data io.Reader, path, _ string) error {
	body, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	f.files[path] = body
	return nil
}

func (f *fakeCDN) Check(_ context.Context, path string) (string, int64, error) {
	body, ok := f.files[path]
	if !ok {
		return "", 0, bunny.ErrFileNotFound
	}
	return "", int64(len(body)), nil
}

func (f *fakeCDN) Download(_ context.Context, path string) (io.ReadCloser, error) {
	body, ok := f.files[path]
	if !ok {
		return nil, bunny.ErrFileNotFound
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (f *fakeCDN) Delete(_ context.Context, path string) error {
	delete(f.files, path)
	return nil
}

func (f *fakeCDN) CDNURLWithRawQuery(path, rawQuery string) string {
	if rawQuery == "" {
		return "https://cdn.test/" + path
	}
	return "https://cdn.test/" + path + "?" + rawQuery
}
