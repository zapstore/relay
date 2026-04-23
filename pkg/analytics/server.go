package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/zapstore/relay/pkg/analytics/store"
)

// API response types

type impressionResponse struct {
	AppID       string `json:"app_id,omitempty"`
	AppPubkey   string `json:"app_pubkey,omitempty"`
	Day         string `json:"day,omitempty"`
	Source      string `json:"source,omitempty"`
	Type        string `json:"type,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	Count       int    `json:"count"`
}

type downloadResponse struct {
	Hash        string `json:"hash,omitempty"`
	Day         string `json:"day,omitempty"`
	Source      string `json:"source,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	Count       int    `json:"count"`
}

type relayMetricsResponse struct {
	Day     string `json:"day"`
	Reqs    int64  `json:"reqs"`
	Filters int64  `json:"filters"`
	Events  int64  `json:"events"`
}

type blossomMetricsResponse struct {
	Day       string `json:"day"`
	Checks    int64  `json:"checks"`
	Downloads int64  `json:"downloads"`
	Uploads   int64  `json:"uploads"`
}

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
//   - app_id     — optional; filter to a specific app
//   - app_pubkey — optional; filter to a specific publisher
//   - from       — YYYY-MM-DD inclusive
//   - to         — YYYY-MM-DD inclusive
//   - source     — optional; filter to a specific source
//   - type       — optional; filter to a specific type
//   - group_by   — comma-separated subset of: app_id, pubkey, day, source, type, country
func (e *Engine) handleImpressions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.ImpressionFilter{
		AppID:     q.Get("app_id"),
		AppPubkey: q.Get("app_pubkey"),
		From:      q.Get("from"),
		To:        q.Get("to"),
		Source:    store.Source(q.Get("source")),
		Type:      store.Type(q.Get("type")),
		GroupBy:   splitCSV(q.Get("group_by")),
	}

	if err := filter.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rows, err := e.store.QueryImpressions(ctx, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := make([]impressionResponse, len(rows))
	for i, r := range rows {
		resp[i] = impressionResponse{
			AppID:       r.AppID,
			AppPubkey:   r.AppPubkey,
			Day:         r.Day,
			Source:      string(r.Source),
			Type:        string(r.Type),
			CountryCode: r.CountryCode,
			Count:       r.Count,
		}
	}
	writeJSON(w, resp)
}

// handleDownloads serves GET /v1/downloads
//
// Query params:
//   - hash       — optional; filter to a specific blob hash
//   - from       — YYYY-MM-DD inclusive
//   - to         — YYYY-MM-DD inclusive
//   - source     — optional; filter to a specific source
//   - group_by   — comma-separated subset of: hash, day, source, country
func (e *Engine) handleDownloads(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.DownloadFilter{
		Hash:    q.Get("hash"),
		From:    q.Get("from"),
		To:      q.Get("to"),
		Source:  store.Source(q.Get("source")),
		GroupBy: splitCSV(q.Get("group_by")),
	}

	if err := filter.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rows, err := e.store.QueryDownloads(ctx, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := make([]downloadResponse, len(rows))
	for i, r := range rows {
		resp[i] = downloadResponse{
			Hash:        r.Hash.Hex(),
			Day:         r.Day,
			Source:      string(r.Source),
			CountryCode: r.CountryCode,
			Count:       r.Count,
		}
	}
	writeJSON(w, resp)
}

// handleRelayMetrics serves GET /v1/metrics/relay
//
// Query params:
//   - from — YYYY-MM-DD inclusive
//   - to   — YYYY-MM-DD inclusive
func (e *Engine) handleRelayMetrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")

	if from != "" {
		if _, err := time.Parse("2006-01-02", from); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if to != "" {
		if _, err := time.Parse("2006-01-02", to); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rows, err := e.store.QueryRelayMetrics(ctx, q.Get("from"), q.Get("to"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := make([]relayMetricsResponse, len(rows))
	for i, r := range rows {
		resp[i] = relayMetricsResponse{
			Day:     r.Day,
			Reqs:    r.Reqs,
			Filters: r.Filters,
			Events:  r.Events,
		}
	}
	writeJSON(w, resp)
}

// handleBlossomMetrics serves GET /v1/metrics/blossom
//
// Query params:
//   - from — YYYY-MM-DD inclusive
//   - to   — YYYY-MM-DD inclusive
func (e *Engine) handleBlossomMetrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")

	if from != "" {
		if _, err := time.Parse("2006-01-02", from); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if to != "" {
		if _, err := time.Parse("2006-01-02", to); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rows, err := e.store.QueryBlossomMetrics(ctx, q.Get("from"), q.Get("to"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := make([]blossomMetricsResponse, len(rows))
	for i, r := range rows {
		resp[i] = blossomMetricsResponse{
			Day:       r.Day,
			Checks:    r.Checks,
			Downloads: r.Downloads,
			Uploads:   r.Uploads,
		}
	}
	writeJSON(w, resp)
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
