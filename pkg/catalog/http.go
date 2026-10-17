package catalog

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/pippellia-btc/rely/v2"
	"golang.org/x/sync/singleflight"

	"github.com/zapstore/relay/pkg/rate"
)

const (
	protocolV1   = "v1"
	unknownEpoch = int64(-1)
)

var (
	protocolPattern = regexp.MustCompile(`^v[0-9]{1,3}$`)
	clientPattern   = regexp.MustCompile(`^[0-9][0-9A-Za-z.+_-]{0,31}$`)
)

// Counter records one finished bundle response.
// from and to are -1 when that epoch was not established.
// protocol is the path version, such as "v1", or empty when the path had none.
// client is the v query. It is empty when v was missing or not a version label.
type Counter interface {
	RecordBundle(from, to int64, status int, protocol, client string)
}

// Handler serves GET /v1/bundle from snapshots and cached bundles.
// Notices returns the operator text for removed apps. Nil means none.
// Limit, when set, costs one token to build a missing bundle for that IP group.
// A 304 and a bundle already on disk are free.
// Shared makes concurrent builds of the same from/to pair one build.
// Count, when set, records each response after its status is chosen.
type Handler struct {
	Data    string
	Signer  Signer
	Notices func(context.Context) (map[string]string, error)
	Log     *slog.Logger
	Limit   rate.Limiter
	Count   Counter

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
	hit := bundleHit{from: unknownEpoch, to: unknownEpoch}
	sw := &statusWriter{ResponseWriter: w}
	defer h.finish(&hit, sw)

	protocol, supported := bundleProtocol(r.URL.Path)
	hit.protocol = protocol
	hit.client = clientLabel(r.URL.Query().Get("v"))
	fromRaw := strings.TrimSpace(r.URL.Query().Get("from"))
	if from, ok := parseFrom(fromRaw); ok {
		hit.from = from
	}

	if r.Method != http.MethodGet {
		http.Error(sw, "GET is required", http.StatusMethodNotAllowed)
		return
	}
	if !supported {
		http.Error(sw, "unsupported catalog-sync version", http.StatusBadRequest)
		return
	}
	if fromRaw == "" {
		http.Error(sw, "from is required", http.StatusBadRequest)
		return
	}
	if hit.from < 0 {
		http.Error(sw, "from must be a non-negative integer", http.StatusBadRequest)
		return
	}
	to, err := LatestSnap(h.Data)
	if err != nil {
		h.logErr(err)
		http.Error(sw, "failed to read catalog epoch", http.StatusInternalServerError)
		return
	}
	hit.to = to
	if hit.from > to {
		http.Error(sw, "from is newer than the current catalog epoch", http.StatusBadRequest)
		return
	}
	if hit.from == to {
		sw.WriteHeader(http.StatusNotModified)
		return
	}
	_, err = os.Stat(BundlePath(h.Data, hit.from, to))
	if err != nil && !os.IsNotExist(err) {
		h.logErr(err)
		http.Error(sw, "failed to read catalog update", http.StatusInternalServerError)
		return
	}
	if os.IsNotExist(err) && h.Limit.Limiter != nil && !h.Limit.Allow(rely.GetIP(r).Group(), 1) {
		sw.WriteHeader(http.StatusTooManyRequests)
		return
	}
	body, err := h.bundle(r.Context(), hit.from, to)
	if err != nil {
		h.logErr(err)
		http.Error(sw, "failed to build catalog update", http.StatusInternalServerError)
		return
	}
	sw.Header().Set("Content-Type", MediaType)
	sw.WriteHeader(http.StatusOK)
	if _, err := sw.Write(body); err != nil {
		h.logErr(err)
	}
}

type bundleHit struct {
	from, to int64
	protocol string
	client   string
}

func (h Handler) finish(hit *bundleHit, sw *statusWriter) {
	if h.Count == nil || sw.code == 0 {
		return
	}
	h.Count.RecordBundle(hit.from, hit.to, sw.code, hit.protocol, hit.client)
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// bundleProtocol returns the path version and whether this relay serves it.
// /v1/bundle is served. Another /vN/bundle is reported and refused.
// A path that is not a version returns an empty protocol.
func bundleProtocol(path string) (string, bool) {
	ver, rest, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if !ok || rest != "bundle" || !protocolPattern.MatchString(ver) {
		return "", false
	}
	return ver, ver == protocolV1
}

func parseFrom(raw string) (int64, bool) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != raw {
		return unknownEpoch, false
	}
	return n, true
}

func clientLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !clientPattern.MatchString(raw) {
		return ""
	}
	return raw
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
