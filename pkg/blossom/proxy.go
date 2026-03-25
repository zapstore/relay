package blossom

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	blossomlib "github.com/pippellia-btc/blossom"
	"github.com/pippellia-btc/blossy"
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/rate"
)

// AssetResolver looks up a download URL for a kind 3063 asset by its SHA-256 hash.
type AssetResolver interface {
	ResolveAssetURL(ctx context.Context, hash string) (url string, found bool, err error)
}

// Proxy returns an http.HandlerFunc that resolves a SHA-256 hash (via the "r" query parameter)
// to the download URL from the corresponding kind 3063 event, records the download in
// analytics, and 307-redirects the client to the asset URL.
//
// Requests with a missing, non-hex, or wrong-length "r" value are rejected with 404
// without touching the event store.
func Proxy(resolver AssetResolver, limiter rate.Limiter, analytics *analytics.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hashHex := r.URL.Query().Get("r")
		if len(hashHex) != 64 {
			http.NotFound(w, r)
			return
		}

		raw, err := hex.DecodeString(hashHex)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		ip := blossy.GetIP(r).Group()
		if !limiter.Allow(ip, 10.0) {
			http.Error(w, ErrRateLimited.Error(), http.StatusTooManyRequests)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		assetURL, found, err := resolver.ResolveAssetURL(ctx, hashHex)
		if err != nil {
			slog.Error("blossom: proxy failed to resolve asset URL", "hash", hashHex, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if !found || assetURL == "" {
			http.NotFound(w, r)
			return
		}

		var hash blossomlib.Hash
		copy(hash[:], raw)

		analytics.RecordProxyDownload(r, hash)

		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.Redirect(w, r, assetURL, http.StatusTemporaryRedirect)
	}
}
