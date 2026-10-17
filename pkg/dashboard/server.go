package dashboard

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pippellia-btc/nwt"
	"github.com/pippellia-btc/rely/v2"
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/blossom"
	"github.com/zapstore/relay/pkg/policy"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay"
	"github.com/zapstore/steroid/artifacts"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

// T serves the dashboard UI.
type T struct {
	template *template.Template
	config   Config
	version  string

	auth      authValidator
	limiter   rate.Limiter
	pol       *policy.Store
	ban       policy.BanFunc
	relay     relay.DB
	blossom   blossom.DB
	analytics analytics.DB
	artifacts *artifacts.DB
	dataDir   string
	seal      SealFunc
	enrich    EnrichFunc
	readOnly  bool
	jobs      runner
}

// New parses the embedded templates and returns a ready-to-use Server.
func New(
	config Config,
	limiter rate.Limiter,
	pol *policy.Store,
	ban policy.BanFunc,
	relay relay.DB,
	blossom blossom.DB,
	analytics analytics.DB,
	arts *artifacts.DB,
	dataDir string,
	seal SealFunc,
	enrichApp EnrichFunc,
) (*T, error) {
	version, err := staticVersion()
	if err != nil {
		return nil, fmt.Errorf("dashboard: hash static files: %w", err)
	}
	tmpl, err := template.New("").Funcs(viewFuncs(version)).ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: failed to parse templates: %w", err)
	}
	return &T{
		template:  tmpl,
		config:    config,
		version:   version,
		auth:      authValidator{config},
		limiter:   limiter,
		pol:       pol,
		ban:       ban,
		relay:     relay,
		blossom:   blossom,
		analytics: analytics,
		artifacts: arts,
		dataDir:   dataDir,
		seal:      seal,
		enrich:    enrichApp,
		readOnly:  config.ReadOnly,
	}, nil
}

// rateLimit is middleware that rate limits requests by IP.
func (d *T) rateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := rely.GetIP(r).Group()
		if !d.limiter.Allow(ip, 1) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

func (d *T) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// Public routes.
	mux.HandleFunc("GET /static/", d.rateLimit(d.static))
	mux.HandleFunc("GET /login", d.rateLimit(d.loginPage))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/apps", http.StatusFound)
	})
	mux.HandleFunc("GET /apps/{id}/icon", d.rateLimit(d.appIcon))

	// Pages: a plain request gets the shell, an HTMX request gets the section.
	mux.HandleFunc("GET /apps", d.rateLimit(d.page("Apps", d.appsPage)))
	mux.HandleFunc("GET /apps/{id}", d.rateLimit(d.page("Apps", d.appPage)))
	mux.HandleFunc("GET /publishers", d.rateLimit(d.page("Publishers", d.publishersPage)))
	mux.HandleFunc("GET /publishers/{id}", d.rateLimit(d.page("Publishers", d.publisherPage)))
	mux.HandleFunc("GET /stats", d.rateLimit(d.page("Stats", d.statsPage)))
	mux.HandleFunc("GET /bundles", d.rateLimit(d.page("Bundles", d.bundlesPage)))
	mux.HandleFunc("GET /moderation", d.rateLimit(d.page("Moderation", d.moderationPage)))

	// Actions.
	mux.HandleFunc("POST /apps/{id}/enrich", d.rateLimit(d.admin(d.enrichApp)))
	mux.HandleFunc("POST /bundles/seal", d.rateLimit(d.admin(d.sealBundle)))
	mux.HandleFunc("POST /bundles/{from}/{to}", d.rateLimit(d.admin(d.deleteBundle)))
	mux.HandleFunc("POST /moderation/ban", d.rateLimit(d.admin(d.addBan)))
	mux.HandleFunc("POST /moderation/unban", d.rateLimit(d.admin(d.removeBan)))
	mux.HandleFunc("POST /moderation/unban-hash", d.rateLimit(d.admin(d.removeHash)))
	mux.HandleFunc("POST /moderation/allow", d.rateLimit(d.admin(d.addAllow)))
	mux.HandleFunc("POST /moderation/disallow", d.rateLimit(d.admin(d.removeAllow)))
	mux.HandleFunc("GET /jobs/{id}", d.rateLimit(d.viewer(d.jobStatus)))
	mux.HandleFunc("POST /jobs/{id}/cancel", d.rateLimit(d.admin(d.cancelJob)))
	return mux
}

// StartAndServe starts the dashboard HTTP server and blocks until ctx is cancelled.
// Running jobs are cancelled and awaited before it returns.
func (d *T) StartAndServe(ctx context.Context, addr string) error {
	defer d.jobs.close()

	server := &http.Server{
		Addr:              addr,
		Handler:           gzipped(d.routes()),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	exit := make(chan error, 1)
	go func() {
		slog.Info("serving the dashboard", "address", addr)
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			exit <- err
		}
	}()

	select {
	case err := <-exit:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutCtx)
	}
}

type pageFunc func(w http.ResponseWriter, r *http.Request, token nwt.Token)

type shellData struct {
	Section string
	Path    string
}

// page serves the shell to a plain request, since the token lives in the
// browser and only HTMX requests carry it. The shell then loads the same URL.
func (d *T) page(section string, render pageFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "HX-Request")
		if r.Header.Get("HX-Request") != "true" {
			d.render(w, "layout", shellData{Section: section, Path: r.URL.RequestURI()})
			return
		}
		token, ok := d.authenticate(w, r)
		if !ok {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		render(w, r, token)
	}
}

func (d *T) viewer(next pageFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := d.authenticate(w, r)
		if !ok {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r, token)
	}
}

func (d *T) admin(next pageFunc) http.HandlerFunc {
	return d.viewer(func(w http.ResponseWriter, r *http.Request, token nwt.Token) {
		if !d.auth.IsAdmin(token) {
			http.Error(w, "forbidden: admin access required", http.StatusForbidden)
			return
		}
		next(w, r, token)
	})
}

func (d *T) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := d.template.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("dashboard: render", "template", name, "error", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// fail writes err as a plain message the shell shows in its toast.
func fail(w http.ResponseWriter, status int, err error) {
	if status >= 500 {
		slog.Error("dashboard: request", "error", err)
	}
	http.Error(w, err.Error(), status)
}

type loginPageData struct {
	Hostname string
}

func (d *T) loginPage(w http.ResponseWriter, r *http.Request) {
	d.render(w, "login", loginPageData{Hostname: d.config.Hostname})
}

// static serves embedded files. A request carrying the current version is
// cached for good; anything else revalidates against the same ETag.
func (d *T) static(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("v") == d.version {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	w.Header().Set("ETag", `"`+d.version+`"`)
	http.FileServerFS(staticFiles).ServeHTTP(w, r)
}

// staticVersion is a short hash of every embedded static file.
func staticVersion() (string, error) {
	h := sha256.New()
	err := fs.WalkDir(staticFiles, "static", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		b, err := staticFiles.ReadFile(path)
		if err != nil {
			return err
		}
		io.WriteString(h, path)
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// gzipped compresses text responses. Fonts and images are already compressed.
func gzipped(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || !compressible(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

func compressible(path string) bool {
	if strings.HasSuffix(path, "/icon") {
		return false
	}
	if rest, ok := strings.CutPrefix(path, "/static/"); ok {
		return strings.HasSuffix(rest, ".css") || strings.HasSuffix(rest, ".js") || strings.HasSuffix(rest, ".svg")
	}
	return true
}

// gzipWriter picks the encoding when the status is known: a 304 or 204 has
// no body, and http.Error clears headers set before it runs.
type gzipWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func (g *gzipWriter) WriteHeader(status int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	if status != http.StatusNotModified && status != http.StatusNoContent {
		g.Header().Del("Content-Length")
		g.Header().Set("Content-Encoding", "gzip")
		g.gz = gzip.NewWriter(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.gz == nil {
		return g.ResponseWriter.Write(b)
	}
	return g.gz.Write(b)
}

func (g *gzipWriter) close() {
	if g.gz != nil {
		// The client has the body or has gone; nothing to recover here.
		_ = g.gz.Close()
	}
}
