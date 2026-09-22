package blossom

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"github.com/zapstore/relay/pkg/blossom/store"
)

func TestFilesPutOpen(t *testing.T) {
	dir := t.TempDir()
	f := Files{Dir: dir}
	payload := []byte("local blossom bytes")
	sum := sha256.Sum256(payload)
	n, err := f.put("blobs/test.bin", bytes.NewReader(payload), sum[:])
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", n, len(payload))
	}

	r, err := f.open("blobs/test.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("read = %q, want %q", got, payload)
	}
}

func TestFilesPutMismatchLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	f := Files{Dir: dir}
	payload := []byte("local blossom bytes")
	want := sha256.Sum256([]byte("other"))
	_, err := f.put("blobs/test.bin", bytes.NewReader(payload), want[:])
	if !errors.Is(err, errChecksumMismatch) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", "test.bin")); !os.IsNotExist(err) {
		t.Fatalf("dest file present: %v", err)
	}
	left, err := filepath.Glob(filepath.Join(dir, "blobs", ".upload-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestFilesRejectsPathEscape(t *testing.T) {
	f := Files{Dir: t.TempDir()}
	if _, err := f.put("../escape.bin", bytes.NewReader([]byte("x")), nil); err == nil {
		t.Fatal("put escaped path succeeded")
	}
}

func TestLocalDownloadServesBytes(t *testing.T) {
	dir := t.TempDir()
	db, err := store.New(filepath.Join(t.TempDir(), "blossom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	payload := []byte("serve me locally")
	hash := blossom.ComputeHash(payload)
	files := Files{Dir: dir}
	if _, err := files.put(BlobPath(hash, "text/plain"), bytes.NewReader(payload), hash[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Save(t.Context(), store.BlobMeta{
		Hash: hash,
		Type: "text/plain",
		Size: int64(len(payload)),
	}); err != nil {
		t.Fatal(err)
	}

	server, err := blossy.NewServer(blossy.WithHostname("localhost"))
	if err != nil {
		t.Fatal(err)
	}
	b := &T{
		server: server,
		config: Config{Dir: dir},
		files:  files,
		store:  db,
	}
	server.On.Check = b.check
	server.On.Download = b.download

	ext := blossom.ExtFromType("text/plain")
	req := httptest.NewRequest(http.MethodGet, "http://localhost/"+hash.Hex()+"."+ext, nil)
	res := httptest.NewRecorder()
	server.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
	if !bytes.Equal(res.Body.Bytes(), payload) {
		t.Fatalf("body = %q, want %q", res.Body.Bytes(), payload)
	}

	head := httptest.NewRequest(http.MethodHead, "http://localhost/"+hash.Hex()+"."+ext, nil)
	headRes := httptest.NewRecorder()
	server.ServeHTTP(headRes, head)
	if headRes.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d", headRes.Code)
	}
}

func TestLocalUploadWritesBytes(t *testing.T) {
	dir := t.TempDir()
	db, err := store.New(filepath.Join(t.TempDir(), "blossom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	payload := []byte("upload me locally")
	hash := blossom.ComputeHash(payload)
	b := &T{
		config: Config{Dir: dir, StallTimeout: 30 * time.Second},
		files:  Files{Dir: dir},
		store:  db,
		relay:  stubRelay{},
	}

	req := httptest.NewRequest(http.MethodPut, "http://localhost/upload", bytes.NewReader(payload))
	hints := blossy.UploadHints{Hash: &hash, Type: "text/plain", Size: int64(len(payload))}
	desc, berr := b.upload(fakeRequest{raw: req, pubkey: "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"}, hints, bytes.NewReader(payload))
	if berr != nil {
		t.Fatalf("upload: %v", berr)
	}
	if desc.Size != int64(len(payload)) || desc.Hash != hash {
		t.Fatalf("descriptor = %+v", desc)
	}

	got, err := os.ReadFile(filepath.Join(dir, BlobPath(hash, "text/plain")))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("stored = %q, want %q", got, payload)
	}
}

type stubRelay struct{}

func (stubRelay) ResolveAssetURL(context.Context, blossom.Hash) (string, error) {
	return "", nil
}

func (stubRelay) NotifyUpload(blossom.Hash, string) error { return nil }

type fakeRequest struct {
	raw    *http.Request
	pubkey string
}

func (f fakeRequest) ID() int64                { return 1 }
func (f fakeRequest) IP() blossy.IP            { return blossy.IP{} }
func (f fakeRequest) Pubkey() string           { return f.pubkey }
func (f fakeRequest) IsAuthed() bool           { return f.pubkey != "" }
func (f fakeRequest) Context() context.Context { return f.raw.Context() }
func (f fakeRequest) Raw() *http.Request       { return f.raw }
