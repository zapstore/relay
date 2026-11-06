package catalog

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pippellia-btc/rely/v2"
	"golang.org/x/sync/singleflight"

	"github.com/zapstore/relay/pkg/rate"
)

const (
	protocolV1   = "v1"
	unknownEpoch = int64(-1)

	// A full bundle holds every app's files in memory while it is built.
	defaultBuilds    = 4
	buildTimeout     = 5 * time.Minute
	busyRetrySeconds = 30
)

var errBusy = errors.New("bundle builds are at capacity")

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
// MaxShare above zero serves 0→to for a delta that ships more than that share of the catalog.
// Shared makes concurrent builds of the same from/to pair one build, with at most
// Builds pairs building at once. Zero Builds means 4.
// Count, when set, records each response after its status is chosen.
type Handler struct {
	Data     string
	Signer   Signer
	Notices  func(context.Context) (map[string]string, error)
	Log      *slog.Logger
	Limit    rate.Limiter
	Count    Counter
	MaxShare float64
	Builds   int

	flight *singleflight.Group
	builds chan struct{}
	build  func(ctx context.Context, from, to int64) error
}

// Shared returns a handler whose concurrent requests for one from/to pair
// build that bundle once, with at most Builds pairs building at a time.
// A failed build is not kept.
func (h Handler) Shared() Handler {
	n := h.Builds
	if n <= 0 {
		n = defaultBuilds
	}
	h.flight = &singleflight.Group{}
	h.builds = make(chan struct{}, n)
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
	from, err := h.servedFrom(hit.from, to)
	if err != nil {
		h.logErr(err)
		http.Error(sw, "failed to read catalog epoch", http.StatusInternalServerError)
		return
	}
	_, err = os.Stat(BundlePath(h.Data, from, to))
	if err != nil && !os.IsNotExist(err) {
		h.logErr(err)
		http.Error(sw, "failed to read catalog update", http.StatusInternalServerError)
		return
	}
	if os.IsNotExist(err) && h.Limit.Limiter != nil {
		ip := rely.GetIP(r).Group()
		if !h.Limit.Allow(ip, 1) {
			wait := h.Limit.RetryAfter(ip, 1)
			sw.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
			sw.WriteHeader(http.StatusTooManyRequests)
			return
		}
	}
	f, err := h.open(r.Context(), from, to)
	if errors.Is(err, errBusy) {
		sw.Header().Set("Retry-After", strconv.Itoa(busyRetrySeconds))
		http.Error(sw, "catalog update is being built, retry later", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		h.logErr(err)
		http.Error(sw, "failed to build catalog update", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		h.logErr(err)
		http.Error(sw, "failed to read catalog update", http.StatusInternalServerError)
		return
	}
	sw.Header().Set("Content-Type", MediaType)
	http.ServeContent(sw, r, "", info.ModTime(), f)
}

// servedFrom is the from of the bundle that answers a request from from.
// It is 0 when from has no snapshot (it predates a rebuild or was pruned) or
// when a build found the from→to delta too large.
func (h Handler) servedFrom(from, to int64) (int64, error) {
	if from == 0 {
		return 0, nil
	}
	if _, err := os.Stat(SnapPath(h.Data, from)); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	if _, err := os.Stat(fullMarkerPath(h.Data, from, to)); err == nil {
		return 0, nil
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	return from, nil
}

// open returns the cached bundle file, building it first when absent.
// A delta the build refuses is served as 0→to.
// A seal can delete a bundle between the build and the open, so a missing file is built once more.
func (h Handler) open(ctx context.Context, from, to int64) (*os.File, error) {
	for attempt := 0; ; attempt++ {
		err := h.bundle(ctx, from, to)
		if errors.Is(err, ErrFullBundle) && from > 0 {
			from = 0
			err = h.bundle(ctx, from, to)
		}
		if err != nil {
			return nil, err
		}
		f, err := os.Open(BundlePath(h.Data, from, to))
		if err == nil || !os.IsNotExist(err) || attempt > 0 {
			return f, err
		}
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

// bundle makes sure the from→to bundle is on disk. A shared handler runs at most
// Builds builds at once and returns errBusy beyond that. A shared build outlives
// the request that started it, because other requests wait on it.
func (h Handler) bundle(ctx context.Context, from, to int64) error {
	if _, err := os.Stat(BundlePath(h.Data, from, to)); err == nil {
		return nil
	}
	build := func(ctx context.Context) error {
		if h.build != nil {
			return h.build(ctx, from, to)
		}
		return WriteBundle(ctx, h.Data, from, to, h.MaxShare, h.Signer, h.Notices)
	}
	if h.flight == nil {
		return build(ctx)
	}
	key := strconv.FormatInt(from, 10) + ":" + strconv.FormatInt(to, 10)
	_, err, _ := h.flight.Do(key, func() (any, error) {
		select {
		case h.builds <- struct{}{}:
			defer func() { <-h.builds }()
		default:
			return nil, errBusy
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), buildTimeout)
		defer cancel()
		return nil, build(ctx)
	})
	return err
}

func (h Handler) logErr(err error) {
	if h.Log != nil && err != nil {
		h.Log.Error("bundle", "error", err)
	}
}
