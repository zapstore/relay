package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/pippellia-btc/rely/v2"
	defender "github.com/zapstore/defender/pkg/client"
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/blossom"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

// T serves the dashboard UI.
type T struct {
	template *template.Template
	config   Config

	auth      authValidator
	limiter   rate.Limiter
	defender  defender.T
	relay     relay.DB
	blossom   blossom.DB
	analytics analytics.DB
}

// New parses the embedded templates and returns a ready-to-use Server.
func New(
	config Config,
	limiter rate.Limiter,
	defender defender.T,
	relay relay.DB,
	blossom blossom.DB,
	analytics analytics.DB,
) (*T, error) {
	funcs := template.FuncMap{
		"json": func(v any) (string, error) {
			b, err := json.Marshal(v)
			return string(b), err
		},

		"truncate": func(n int, s string) string {
			runes := []rune(s)
			if len(runes) <= n {
				return s
			}
			return string(runes[:n]) + "…"
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: failed to parse templates: %w", err)
	}
	return &T{
		template:  tmpl,
		config:    config,
		auth:      authValidator{config},
		limiter:   limiter,
		defender:  defender,
		relay:     relay,
		blossom:   blossom,
		analytics: analytics,
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

// StartAndServe starts the dashboard HTTP server and blocks until ctx is cancelled.
func (d *T) StartAndServe(ctx context.Context, addr string) error {
	mux := http.NewServeMux()

	// Public routes.
	mux.Handle("GET /static/", d.rateLimit(http.FileServerFS(staticFiles).ServeHTTP))
	mux.HandleFunc("GET /{$}", d.rateLimit(d.index))
	mux.HandleFunc("GET /login", d.rateLimit(d.loginPage))

	mux.HandleFunc("GET /tabs/apps", d.rateLimit(d.appsPage))
	mux.HandleFunc("GET /tabs/apps/chart", d.rateLimit(d.appChartEndpoint))
	mux.HandleFunc("GET /tabs/apps/ranking", d.rateLimit(d.appRankingEndpoint))

	mux.HandleFunc("GET /tabs/relay", d.rateLimit(d.relayPage))
	mux.HandleFunc("GET /tabs/blossom", d.rateLimit(d.blossomPage))

	mux.HandleFunc("GET /tabs/defender", d.rateLimit(d.defenderPage))
	mux.HandleFunc("POST /defender/policies", d.rateLimit(d.createPolicy))
	mux.HandleFunc("DELETE /defender/policies", d.rateLimit(d.deletePolicy))

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
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

func (d *T) index(w http.ResponseWriter, r *http.Request) {
	if err := d.template.ExecuteTemplate(w, "layout", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type loginPageData struct {
	Hostname string
}

func (d *T) loginPage(w http.ResponseWriter, r *http.Request) {
	data := loginPageData{Hostname: d.config.Hostname}
	if err := d.template.ExecuteTemplate(w, "login", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
