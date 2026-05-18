package dashboard

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"
)

//go:embed templates/*.html
var templateFiles embed.FS

// Server serves the dashboard UI.
type Server struct {
	tmpl *template.Template
}

// New parses the embedded templates and returns a ready-to-use Server.
func New() (*Server, error) {
	tmpl, err := template.ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: failed to parse templates: %w", err)
	}
	return &Server{tmpl: tmpl}, nil
}

// StartAndServe starts the dashboard HTTP server and blocks until ctx is cancelled.
func (s *Server) StartAndServe(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /tabs/overview", s.overview)
	mux.HandleFunc("GET /tabs/apps", s.apps)
	mux.HandleFunc("GET /tabs/defender", s.defender)

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

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if err := s.tmpl.ExecuteTemplate(w, "layout", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	if err := s.tmpl.ExecuteTemplate(w, "overview", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) apps(w http.ResponseWriter, r *http.Request) {
	if err := s.tmpl.ExecuteTemplate(w, "apps", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) defender(w http.ResponseWriter, r *http.Request) {
	if err := s.tmpl.ExecuteTemplate(w, "defender", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
