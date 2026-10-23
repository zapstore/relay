package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	catalog "github.com/zapstore/relay/pkg/catalog"
)

func TestBundleFilter(t *testing.T) {
	got, err := bundleFilter([]string{"relay", "bundle"})
	if err != nil || got != "" {
		t.Fatalf("bare bundle: %q %v", got, err)
	}
	got, err = bundleFilter([]string{"relay", "bundle", " org.mozilla "})
	if err != nil || got != "org.mozilla" {
		t.Fatalf("filter: %q %v", got, err)
	}
	if _, err := bundleFilter([]string{"relay", "bundle", "  "}); err == nil {
		t.Fatal("empty filter accepted")
	}
	if _, err := bundleFilter([]string{"relay", "bundle", "org.mozilla", "extra"}); err == nil {
		t.Fatal("extra arg accepted")
	}
}

func TestWrongMethodIs405(t *testing.T) {
	ws := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	mux := routes(catalog.Handler{Data: t.TempDir()}, ws)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/bundle"},
		{http.MethodPost, "/v2/bundle"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d", tc.method, tc.path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/bundle?from=0", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("GET /v2/bundle = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/search?q=x", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("GET /v1/search = %d, want the WebSocket handler", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/bundle", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("GET /bundle = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("GET / = %d", rec.Code)
	}
}
