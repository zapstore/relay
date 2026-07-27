package blossom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/blossom"
	"github.com/pippellia-btc/blossy"
	"github.com/zapstore/relay/pkg/events"
)

type stubRelay struct {
	events []nostr.Event
	err    error
}

func (s stubRelay) ResolveAssetURL(context.Context, blossom.Hash) (string, error) {
	return "", nil
}

func (s stubRelay) Query(_ context.Context, filter nostr.Filter) ([]nostr.Event, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []nostr.Event
	for _, e := range s.events {
		if len(filter.Kinds) > 0 {
			match := false
			for _, k := range filter.Kinds {
				if e.Kind == k {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		if len(filter.Authors) > 0 {
			match := false
			for _, a := range filter.Authors {
				if e.PubKey == a {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		if ids := filter.Tags["i"]; len(ids) > 0 {
			appID, ok := events.Find(e.Tags, "i")
			if !ok || appID != ids[0] {
				continue
			}
		}
		out = append(out, e)
	}
	// Newest first, matching nostr-sqlite ORDER BY created_at DESC.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt > out[i].CreatedAt {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (s stubRelay) NotifyUpload(blossom.Hash, string) error {
	return nil
}

func zapstoreAssetEvent(hash blossom.Hash, version string, createdAt nostr.Timestamp) nostr.Event {
	return nostr.Event{
		PubKey:    zapstorePubkey,
		CreatedAt: createdAt,
		Kind:      events.KindAsset,
		Tags: nostr.Tags{
			{"i", zapstoreAppID},
			{"x", hash.Hex()},
			{"version", version},
		},
	}
}

func TestDownloadLatest(t *testing.T) {
	payload := []byte("zapstore-apk")
	hash := blossom.ComputeHash(payload)
	version := "1.2.3"

	server, err := blossy.NewServer(blossy.WithHostname("cdn.example.com"))
	if err != nil {
		t.Fatal(err)
	}

	var gotClient, gotType string
	var gotHash blossom.Hash
	var gotInline bool
	server.On.Download = func(r blossy.Request, h blossom.Hash, _ string) (blossy.BlobDelivery, *blossom.Error) {
		gotClient = r.Raw().Header.Get("X-Zapstore-Client")
		gotType = r.Raw().Header.Get("X-Zapstore-Download-Type")
		gotHash = h
		gotInline = serveInline(r.Context())
		return blossy.Serve(blossom.BlobFromBytes(payload)), nil
	}

	b := &T{
		server: server,
		relay:  stubRelay{events: []nostr.Event{zapstoreAssetEvent(hash, version, 1700001000)}},
	}

	req := httptest.NewRequest(http.MethodGet, "https://cdn.example.com/download-latest", nil)
	// Client-supplied values must be overwritten.
	req.Header.Set("X-Zapstore-Client", "app")
	req.Header.Set("X-Zapstore-Download-Type", "update")
	req.Header.Set("Origin", "https://zapstore.dev")
	res := httptest.NewRecorder()

	b.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
	if got := res.Header().Get("Location"); got != "" {
		t.Fatalf("Location = %q, want empty (no redirect)", got)
	}
	if string(res.Body.Bytes()) != string(payload) {
		t.Fatalf("body = %q, want %q", res.Body.Bytes(), payload)
	}
	if gotHash != hash {
		t.Fatalf("hash = %s, want %s", gotHash.Hex(), hash.Hex())
	}
	if !gotInline {
		t.Fatal("expected serve-inline context on GET /download-latest")
	}
	if gotClient != "web" {
		t.Fatalf("X-Zapstore-Client = %q, want web", gotClient)
	}
	if gotType != "install" {
		t.Fatalf("X-Zapstore-Download-Type = %q, want install", gotType)
	}
	assertLatestHeaders(t, res.Header(), version, hash, "https://zapstore.dev")
}

func TestDownloadLatest_HEAD(t *testing.T) {
	hash := blossom.ComputeHash([]byte("zapstore-apk"))
	version := "1.2.3"
	const size int64 = 42_000

	server, err := blossy.NewServer(blossy.WithHostname("cdn.example.com"))
	if err != nil {
		t.Fatal(err)
	}

	var downloadCalled bool
	server.On.Download = func(blossy.Request, blossom.Hash, string) (blossy.BlobDelivery, *blossom.Error) {
		downloadCalled = true
		return nil, blossom.ErrInternal("download should not run on HEAD")
	}
	server.On.Check = func(_ blossy.Request, h blossom.Hash, _ string) (blossy.MetaDelivery, *blossom.Error) {
		if h != hash {
			t.Fatalf("check hash = %s, want %s", h.Hex(), hash.Hex())
		}
		return blossy.Found("application/vnd.android.package-archive", size), nil
	}

	b := &T{
		server: server,
		relay:  stubRelay{events: []nostr.Event{zapstoreAssetEvent(hash, version, 1700001000)}},
	}

	req := httptest.NewRequest(http.MethodHead, "https://cdn.example.com/download-latest", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	res := httptest.NewRecorder()
	b.Handler().ServeHTTP(res, req)

	if downloadCalled {
		t.Fatal("expected HEAD to use check, not download")
	}
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
	if len(res.Body.Bytes()) != 0 {
		t.Fatalf("HEAD body length = %d, want 0", len(res.Body.Bytes()))
	}
	if got := res.Header().Get("Content-Length"); got != "42000" {
		t.Fatalf("Content-Length = %q, want 42000", got)
	}
	assertLatestHeaders(t, res.Header(), version, hash, "http://localhost:5173")
}

func TestDownloadLatest_NotFound(t *testing.T) {
	server, err := blossy.NewServer(blossy.WithHostname("cdn.example.com"))
	if err != nil {
		t.Fatal(err)
	}

	b := &T{
		server: server,
		relay:  stubRelay{},
	}

	req := httptest.NewRequest(http.MethodGet, "https://cdn.example.com/download-latest", nil)
	res := httptest.NewRecorder()
	b.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNotFound)
	}
}

func TestLatestZapstoreAsset(t *testing.T) {
	older := blossom.ComputeHash([]byte("older"))
	newer := blossom.ComputeHash([]byte("newer"))

	b := &T{relay: stubRelay{events: []nostr.Event{
		zapstoreAssetEvent(older, "1.0.0", 1700000000),
		zapstoreAssetEvent(newer, "1.1.0", 1700001000),
		{
			PubKey:    zapstorePubkey,
			CreatedAt: nostr.Timestamp(1700002000),
			Kind:      events.KindAsset,
			Tags: nostr.Tags{
				{"i", "com.example.other"},
				{"x", blossom.ComputeHash([]byte("other")).Hex()},
				{"version", "9.9.9"},
			},
		},
	}}}

	got, err := b.latestZapstoreAsset(context.Background())
	if err != nil {
		t.Fatalf("latestZapstoreAsset: %v", err)
	}
	if got.Hash != newer {
		t.Fatalf("hash = %s, want %s", got.Hash.Hex(), newer.Hex())
	}
	if got.Version != "1.1.0" {
		t.Fatalf("version = %q, want 1.1.0", got.Version)
	}
}

func TestLatestZapstoreAsset_NotFound(t *testing.T) {
	b := &T{relay: stubRelay{}}
	got, err := b.latestZapstoreAsset(context.Background())
	if err != nil {
		t.Fatalf("latestZapstoreAsset: %v", err)
	}
	if got.Hash != (blossom.Hash{}) {
		t.Fatalf("expected zero hash, got %s", got.Hash.Hex())
	}
}

func TestAllowedCORSOrigin(t *testing.T) {
	tests := []struct {
		origin string
		want   bool
	}{
		{"https://zapstore.dev", true},
		{"https://www.zapstore.dev", true},
		{"https://app.zapstore.dev", true},
		{"http://localhost", true},
		{"http://localhost:5173", true},
		{"http://127.0.0.1:3000", true},
		{"https://localhost:5173", true},
		{"http://zapstore.dev", false},
		{"https://evil.com", false},
		{"https://zapstore.dev.evil.com", false},
		{"https://notzapstore.dev", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := allowedCORSOrigin(tt.origin); got != tt.want {
			t.Fatalf("allowedCORSOrigin(%q) = %v, want %v", tt.origin, got, tt.want)
		}
	}
}

func assertLatestHeaders(t *testing.T, h http.Header, version string, hash blossom.Hash, wantOrigin string) {
	t.Helper()
	wantDisposition := `attachment; filename="zapstore-` + version + `.apk"`
	if got := h.Get("Content-Disposition"); got != wantDisposition {
		t.Fatalf("Content-Disposition = %q, want %q", got, wantDisposition)
	}
	if got := h.Get("X-Zapstore-Version"); got != version {
		t.Fatalf("X-Zapstore-Version = %q, want %q", got, version)
	}
	if got := h.Get("X-Zapstore-Sha256"); got != hash.Hex() {
		t.Fatalf("X-Zapstore-Sha256 = %q, want %q", got, hash.Hex())
	}
	wantExpose := "Content-Disposition, Content-Length, X-Zapstore-Version, X-Zapstore-Sha256"
	if got := h.Get("Access-Control-Expose-Headers"); got != wantExpose {
		t.Fatalf("Access-Control-Expose-Headers = %q, want %q", got, wantExpose)
	}
	if got := h.Get("Access-Control-Allow-Origin"); got != wantOrigin {
		t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, wantOrigin)
	}
}
