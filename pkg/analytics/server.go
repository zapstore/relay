package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/zapstore/relay/pkg/analytics/store"
)

// StartAndServe starts the analytics HTTP API and blocks until ctx is cancelled.
func (e *Engine) StartAndServe(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/impressions", e.handleImpressions)
	mux.HandleFunc("GET /v1/downloads", e.handleDownloads)
	mux.HandleFunc("GET /v1/metrics/relay", e.handleRelayMetrics)
	mux.HandleFunc("GET /v1/metrics/blossom", e.handleBlossomMetrics)

	server := &http.Server{Addr: addr, Handler: mux}
	exit := make(chan error, 1)

	go func() {
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			exit <- err
		}
	}()

	select {
	case err := <-exit:
		return err
	case <-ctx.Done():
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(ctx)
	}
}

// handleImpressions serves GET /v1/impressions
//
// Query params:
//   - pubkey     — optional; filter to a specific publisher
//   - from       — YYYY-MM-DD inclusive
//   - to         — YYYY-MM-DD inclusive
//   - group_by   — comma-separated subset of: app_id, pubkey, day, source, type, country
func (e *Engine) handleImpressions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.ImpressionFilter{
		Pubkey:  q.Get("pubkey"),
		From:    q.Get("from"),
		To:      q.Get("to"),
		GroupBy: splitCSV(q.Get("group_by")),
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rows, err := e.store.QueryImpressions(ctx, filter)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, rows)
}

// handleDownloads serves GET /v1/downloads
//
// Query params:
//   - hash       — optional; filter to a specific blob hash
//   - from       — YYYY-MM-DD inclusive
//   - to         — YYYY-MM-DD inclusive
//   - group_by   — comma-separated subset of: hash, day, source, country
func (e *Engine) handleDownloads(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.DownloadFilter{
		Hash:    q.Get("hash"),
		From:    q.Get("from"),
		To:      q.Get("to"),
		GroupBy: splitCSV(q.Get("group_by")),
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rows, err := e.store.QueryDownloads(ctx, filter)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, rows)
}

// handleRelayMetrics serves GET /v1/metrics/relay
//
// Query params:
//   - from — YYYY-MM-DD inclusive
//   - to   — YYYY-MM-DD inclusive
func (e *Engine) handleRelayMetrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rows, err := e.store.QueryRelayMetrics(ctx, q.Get("from"), q.Get("to"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, rows)
}

// handleBlossomMetrics serves GET /v1/metrics/blossom
//
// Query params:
//   - from — YYYY-MM-DD inclusive
//   - to   — YYYY-MM-DD inclusive
func (e *Engine) handleBlossomMetrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rows, err := e.store.QueryBlossomMetrics(ctx, q.Get("from"), q.Get("to"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, rows)
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encoding error", http.StatusInternalServerError)
	}
}

func httpError(w http.ResponseWriter, err error) {
	// Treat unknown group_by as a client error, everything else as internal.
	if strings.Contains(err.Error(), "unknown group_by") {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Error(w, "internal error", http.StatusInternalServerError)
}
