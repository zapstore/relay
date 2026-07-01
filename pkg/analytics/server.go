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

// API response types

type impressionResponse struct {
	AppID       string `json:"app_id,omitempty"`
	AppPubkey   string `json:"app_pubkey,omitempty"`
	AppVersion  string `json:"app_version,omitempty"`
	Day         string `json:"day,omitempty"`
	Source      string `json:"source,omitempty"`
	Type        string `json:"type,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	Count       int    `json:"count"`
}

type downloadResponse struct {
	Hash        string `json:"hash,omitempty"`
	AppID       string `json:"app_id,omitempty"`
	AppVersion  string `json:"app_version,omitempty"`
	AppPubkey   string `json:"app_pubkey,omitempty"`
	Day         string `json:"day,omitempty"`
	Source      string `json:"source,omitempty"`
	Type        string `json:"type,omitempty"`
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
	mux.HandleFunc("GET /v1/app/impressions", e.appImpressions)
	mux.HandleFunc("GET /v1/app/downloads", e.appDownloads)
	mux.HandleFunc("POST /v1/app/downloads", e.appBatchDownload)
	mux.HandleFunc("GET /v1/metrics/relay", e.relayMetrics)
	mux.HandleFunc("GET /v1/metrics/blossom", e.blossomMetrics)

	exit := make(chan error, 1)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		slog.Info("serving the analytics server", "address", addr)
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

// appImpressions serves GET /v1/app/impressions
//
// Query params:
//   - app_id     — optional; filter to a specific app
//   - app_pubkey — optional; filter to a specific publisher
//   - from       — YYYY-MM-DD inclusive
//   - to         — YYYY-MM-DD inclusive
//   - source     — optional; filter to a specific source
//   - type       — optional; filter to a specific type
//   - group_by   — comma-separated subset of: app_id, app_pubkey, app_version, day, source, type, country_code
func (e *Engine) appImpressions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.ImpressionFilter{
		AppID:     q.Get("app_id"),
		AppPubkey: q.Get("app_pubkey"),
		From:      q.Get("from"),
		To:        q.Get("to"),
		Source:    store.Source(q.Get("source")),
		Type:      store.ImpressionType(q.Get("type")),
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
			AppVersion:  r.AppVersion,
			Day:         r.Day,
			Source:      string(r.Source),
			Type:        string(r.Type),
			CountryCode: r.CountryCode,
			Count:       r.Count,
		}
	}
	writeJSON(w, resp)
}

// appBatchDownload serves POST /v1/app/downloads
//
// Request body: {"app_ids": ["com.example.app1", ...], "from": "YYYY-MM-DD", "to": "YYYY-MM-DD"}
// Response:     {"com.example.app1": 42, ...} — count per app ID, 0 if no downloads
func (e *Engine) appBatchDownload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AppIDs []string `json:"app_ids"`
		From   string   `json:"from"`
		To     string   `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(body.AppIDs) == 0 {
		http.Error(w, "app_ids must not be empty", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	counts, err := e.store.QueryDownloadsByAppIDs(ctx, body.AppIDs, body.From, body.To)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, counts)
}

// appDownloads serves GET /v1/app/downloads
//
// Query params:
//   - hash        — optional; filter to a specific blob hash
//   - app_id      — optional; filter to a specific app
//   - app_pubkey  — optional; filter to a specific publisher
//   - from        — YYYY-MM-DD inclusive
//   - to          — YYYY-MM-DD inclusive
//   - source      — optional; filter to a specific source
//   - type        — optional; filter to a specific download type
//   - group_by    — comma-separated subset of: hash, app_id, app_version, app_pubkey, day, source, type, country_code
func (e *Engine) appDownloads(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.DownloadFilter{
		Hash:      q.Get("hash"),
		AppID:     q.Get("app_id"),
		AppPubkey: q.Get("app_pubkey"),
		From:      q.Get("from"),
		To:        q.Get("to"),
		Source:    store.Source(q.Get("source")),
		Type:      store.DownloadType(q.Get("type")),
		GroupBy:   splitCSV(q.Get("group_by")),
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
			AppID:       r.AppID,
			AppVersion:  r.AppVersion,
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

// relayMetrics serves GET /v1/metrics/relay
//
// Query params:
//   - from — YYYY-MM-DD inclusive
//   - to   — YYYY-MM-DD inclusive
func (e *Engine) relayMetrics(w http.ResponseWriter, r *http.Request) {
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

// blossomMetrics serves GET /v1/metrics/blossom
//
// Query params:
//   - from — YYYY-MM-DD inclusive
//   - to   — YYYY-MM-DD inclusive
func (e *Engine) blossomMetrics(w http.ResponseWriter, r *http.Request) {
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
