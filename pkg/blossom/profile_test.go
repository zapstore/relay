package blossom

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pippellia-btc/blossy"
	"github.com/zapstore/relay/pkg/blossom/bunny"
)

func TestProfileRedirect(t *testing.T) {
	server, err := blossy.NewServer(blossy.WithHostname("blossom.example.com"))
	if err != nil {
		t.Fatal(err)
	}

	b := &T{
		server: server,
		bunny:  bunny.NewClient(bunny.Config{CDN: "cdn.example.com"}),
	}
	server.On.Download = b.download
	server.On.Check = b.check

	pubkey := "78ce6faa72264387284e647ba6938995735ec8c7d5c5a65737e55130f026307d"
	wantURL := "https://cdn.example.com/p/" + pubkey + ".webp?class=avatar"

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "https://blossom.example.com/"+pubkey+"."+profileExt+"?class=avatar", nil)
			res := httptest.NewRecorder()

			server.ServeHTTP(res, req)

			if res.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status = %d, want %d", res.Code, http.StatusTemporaryRedirect)
			}
			if got := res.Header().Get("Location"); got != wantURL {
				t.Fatalf("Location = %q, want %q", got, wantURL)
			}
		})
	}
}
