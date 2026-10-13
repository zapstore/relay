package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	catalog "github.com/zapstore/relay/pkg/catalog"
	"github.com/zapstore/relay/pkg/policy"
	"github.com/zapstore/relay/pkg/rate"
)

func TestWrongMethodIs405(t *testing.T) {
	ws := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	mux := routes(
		catalog.Handler{Data: t.TempDir()},
		policy.Suggest(nil, rate.NewLimiter(rate.NewConfig())),
		ws,
	)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/bundle"},
		{http.MethodGet, "/suggest"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d", tc.method, tc.path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("GET / = %d", rec.Code)
	}
}
