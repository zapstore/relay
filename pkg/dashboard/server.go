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

	defender "github.com/zapstore/defender/pkg/client"
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/blossom"
	"github.com/zapstore/relay/pkg/relay"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

// Server serves the dashboard UI.
type T struct {
	template  *template.Template
	relay     relay.DB
	blossom   blossom.DB
	analytics analytics.DB
	defender  defender.T
}

// New parses the embedded templates and returns a ready-to-use Server.
func New(
	relay relay.DB,
	blossom blossom.DB,
	analytics analytics.DB,
	defender defender.T,
) (*T, error) {
	funcs := template.FuncMap{
		"json": func(v any) (string, error) {
			b, err := json.Marshal(v)
			return string(b), err
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: failed to parse templates: %w", err)
	}
	return &T{
		template:  tmpl,
		relay:     relay,
		blossom:   blossom,
		analytics: analytics,
		defender:  defender,
	}, nil
}

// StartAndServe starts the dashboard HTTP server and blocks until ctx is cancelled.
func (d *T) StartAndServe(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(staticFiles))
	mux.HandleFunc("GET /{$}", d.index)
	mux.HandleFunc("GET /tabs/relay", d.relayPage)
	mux.HandleFunc("GET /tabs/apps", d.appsPage)
	mux.HandleFunc("GET /tabs/defender", d.defenderPage)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	exit := make(chan error, 1)
	go func() {
		slog.Info("serving the dashboard", "address", addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			exit <- err
		}
	}()

	select {
	case err := <-exit:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

func (d *T) index(w http.ResponseWriter, r *http.Request) {
	if err := d.template.ExecuteTemplate(w, "layout", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
