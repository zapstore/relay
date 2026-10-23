package relay

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"1.1.1.1":              true,
		"2606:4700:4700::1111": true,
		"127.0.0.1":            false,
		"10.0.0.5":             false,
		"172.16.0.1":           false,
		"192.168.1.1":          false,
		"169.254.169.254":      false,
		"100.64.0.1":           false,
		"0.0.0.0":              false,
		"::1":                  false,
		"fd00::1":              false,
		"fe80::1":              false,
		"::ffff:127.0.0.1":     false,
		"64:ff9b::a00:1":       false,
		"2002:c0a8:101::1":     false,
		"255.255.255.255":      false,
		"224.0.0.1":            false,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: public %v, want %v", addr, got, want)
		}
	}
}

func TestPublicFetchRefusesLoopback(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("apk"))
	}))
	t.Cleanup(srv.Close)
	_, err := publicFetch.Get(srv.URL)
	if !errors.Is(err, errNotPublic) {
		t.Fatalf("got %v, want %v", err, errNotPublic)
	}
}

func TestPublicFetchRefusesHTTPRedirect(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/app.apk", nil)
	if err := publicFetch.CheckRedirect(req, []*http.Request{{}}); !errors.Is(err, errNotHTTPS) {
		t.Fatalf("got %v, want %v", err, errNotHTTPS)
	}
}

func TestAssetURLTakesPublicHTTPSOnly(t *testing.T) {
	r := &T{config: Config{BlobHosts: []string{"cdn.example.com", "", "zone.b-cdn.net"}}}
	tags := nostr.Tags{
		{"url", "http://github.com/a/app.apk"},
		{"url", "https://CDN.example.com/abc.apk"},
		{"url", "https://zone.b-cdn.net/abc.apk"},
		{"url", "file:///etc/passwd"},
		{"url", "https://github.com/a/app.apk"},
	}
	if got := r.assetURL(tags); got != "https://github.com/a/app.apk" {
		t.Fatalf("got %q", got)
	}
	if got := r.assetURL(nostr.Tags{{"url", "http://github.com/a/app.apk"}}); got != "" {
		t.Fatalf("http url accepted: %q", got)
	}
}
