package catalog

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/pippellia-btc/rely/v2"
	"golang.org/x/sync/singleflight"

	"github.com/zapstore/relay/pkg/rate"
)

// Handler serves GET /bundle from snapshots and cached bundles.
// Notices returns the operator text for removed apps. Nil means none.
// Limit, when set, costs one token to build a missing bundle for that IP group.
// A 304 and a bundle already on disk are free.
// Shared makes concurrent builds of the same from/to pair one build.
type Handler struct {
	Data    string
	Signer  Signer
	Notices func(context.Context) (map[string]string, error)
	Log     *slog.Logger
	Limit   rate.Limiter

	flight *singleflight.Group
	build  func(ctx context.Context, from, to int64) ([]byte, error)
}

// Shared returns a handler whose concurrent requests for one from/to pair
// build that bundle once. A failed build is not kept.
func (h Handler) Shared() Handler {
	h.flight = &singleflight.Group{}
	return h
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET is required", http.StatusMethodNotAllowed)
		return
	}
	fromRaw := strings.TrimSpace(r.URL.Query().Get("from"))
	if fromRaw == "" {
		http.Error(w, "from is required", http.StatusBadRequest)
		return
	}
	from, err := strconv.ParseInt(fromRaw, 10, 64)
	if err != nil || from < 0 || strconv.FormatInt(from, 10) != fromRaw {
		http.Error(w, "from must be a non-negative integer", http.StatusBadRequest)
		return
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("v")); raw != "" && raw != "1" {
		http.Error(w, "unsupported catalog-sync version", http.StatusBadRequest)
		return
	}
	to, err := LatestSnap(h.Data)
	if err != nil {
		h.logErr(err)
		http.Error(w, "failed to read catalog epoch", http.StatusInternalServerError)
		return
	}
	if from > to {
		http.Error(w, "from is newer than the current catalog epoch", http.StatusBadRequest)
		return
	}
	if from == to {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, err = os.Stat(BundlePath(h.Data, from, to))
	if err != nil && !os.IsNotExist(err) {
		h.logErr(err)
		http.Error(w, "failed to read catalog update", http.StatusInternalServerError)
		return
	}
	if os.IsNotExist(err) && h.Limit.Limiter != nil && !h.Limit.Allow(rely.GetIP(r).Group(), 1) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	body, err := h.bundle(r.Context(), from, to)
	if err != nil {
		h.logErr(err)
		http.Error(w, "failed to build catalog update", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", MediaType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		h.logErr(err)
	}
}

func (h Handler) bundle(ctx context.Context, from, to int64) ([]byte, error) {
	load := func() ([]byte, error) {
		if h.build != nil {
			return h.build(ctx, from, to)
		}
		return EnsureBundle(ctx, h.Data, from, to, h.Signer, h.Notices)
	}
	if h.flight == nil {
		return load()
	}
	key := strconv.FormatInt(from, 10) + ":" + strconv.FormatInt(to, 10)
	v, err, _ := h.flight.Do(key, func() (any, error) {
		return load()
	})
	if err != nil {
		return nil, err
	}
	body, _ := v.([]byte)
	return body, nil
}

func (h Handler) logErr(err error) {
	if h.Log != nil && err != nil {
		h.Log.Error("bundle", "error", err)
	}
}
