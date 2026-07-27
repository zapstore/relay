package blossom

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/blossom"
	"github.com/zapstore/relay/pkg/events"
)

const (
	zapstoreAppID  = "dev.zapstore.app"
	zapstorePubkey = "78ce6faa72264387284e647ba6938995735ec8c7d5c5a65737e55130f026307d"
)

type latestAsset struct {
	Hash    blossom.Hash
	Version string
}

type serveInlineContextKey struct{}

func withServeInline(ctx context.Context) context.Context {
	return context.WithValue(ctx, serveInlineContextKey{}, true)
}

func serveInline(ctx context.Context) bool {
	v, _ := ctx.Value(serveInlineContextKey{}).(bool)
	return v
}

// allowedCORSOrigin reports whether Origin may read download-latest response headers.
// Allowed: https://zapstore.dev, https://*.zapstore.dev, and localhost / 127.0.0.1.
func allowedCORSOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" {
		return true
	}
	if u.Scheme != "https" {
		return false
	}
	return host == "zapstore.dev" || strings.HasSuffix(host, ".zapstore.dev")
}

// allowOriginWriter overrides blossy's Access-Control-Allow-Origin: * so
// cross-origin reads of download-latest metadata are limited to allowed origins.
// allowOrigin is the reflected Origin when allowed, or empty to strip ACAO.
type allowOriginWriter struct {
	http.ResponseWriter
	allowOrigin string
}

func (w *allowOriginWriter) applyCORS() {
	if w.allowOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", w.allowOrigin)
		return
	}
	w.Header().Del("Access-Control-Allow-Origin")
}

func (w *allowOriginWriter) WriteHeader(code int) {
	w.applyCORS()
	w.ResponseWriter.WriteHeader(code)
}

func (w *allowOriginWriter) Write(p []byte) (int, error) {
	w.applyCORS()
	return w.ResponseWriter.Write(p)
}

// downloadLatest resolves the latest Zapstore APK and serves it through the normal
// blossom download/check path. On both HEAD and GET it exposes version, hash, and
// disposition headers for the webapp; on GET it forces web/install analytics headers
// and streams the APK bytes (no CDN redirect) so Content-Disposition applies.
func (b *T) downloadLatest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	asset, err := b.latestZapstoreAsset(ctx)
	if err != nil {
		slog.Error("blossom: failed to resolve latest zapstore asset", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if asset.Hash == (blossom.Hash{}) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="zapstore-%s.apk"`, asset.Version))
	w.Header().Set("X-Zapstore-Version", asset.Version)
	w.Header().Set("X-Zapstore-Sha256", asset.Hash.Hex())
	w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition, Content-Length, X-Zapstore-Version, X-Zapstore-Sha256")

	if r.Method == http.MethodGet {
		r.Header.Set("X-Zapstore-Client", "web")
		r.Header.Set("X-Zapstore-Download-Type", "install")
		r = r.WithContext(withServeInline(r.Context()))
	}

	allowOrigin := ""
	if origin := r.Header.Get("Origin"); allowedCORSOrigin(origin) {
		allowOrigin = origin
	}

	r.URL.Path = "/" + asset.Hash.Hex()
	r.URL.RawPath = ""
	b.server.ServeHTTP(&allowOriginWriter{ResponseWriter: w, allowOrigin: allowOrigin}, r)
}

// latestZapstoreAsset returns the SHA-256 hash and version from the newest kind 3063
// asset for the Zapstore Android app published by the indexer. A zero hash with a
// nil error means none was found.
func (b *T) latestZapstoreAsset(ctx context.Context) (latestAsset, error) {
	filter := nostr.Filter{
		Kinds:   []int{events.KindAsset},
		Authors: []string{zapstorePubkey},
		Tags:    nostr.TagMap{"i": []string{zapstoreAppID}},
		Limit:   1,
	}
	found, err := b.relay.Query(ctx, filter)
	if err != nil {
		return latestAsset{}, err
	}
	if len(found) == 0 {
		return latestAsset{}, nil
	}

	xTag, ok := events.Find(found[0].Tags, "x")
	if !ok {
		return latestAsset{}, fmt.Errorf("asset missing x tag")
	}
	version, ok := events.Find(found[0].Tags, "version")
	if !ok || version == "" {
		return latestAsset{}, fmt.Errorf("asset missing version tag")
	}
	hash, err := blossom.ParseHash(xTag)
	if err != nil {
		return latestAsset{}, err
	}
	return latestAsset{Hash: hash, Version: version}, nil
}
