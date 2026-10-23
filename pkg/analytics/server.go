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

type relayMetricsResponse struct {
	Day     string `json:"day"`
	Reqs    int64  `json:"reqs"`
	Filters int64  `json:"filters"`
	Events  int64  `json:"events"`
}

type bundleResponse struct {
	Day           string `json:"day,omitempty"`
	FromEpoch     *int64 `json:"from_epoch,omitempty"`
	ToEpoch       *int64 `json:"to_epoch,omitempty"`
	Status        *int   `json:"status,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
	Count         int    `json:"count"`
}

// StartAndServe starts the analytics HTTP API and blocks until ctx is cancelled.
func (e *Engine) StartAndServe(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/metrics/relay", e.relayMetrics)
	mux.HandleFunc("GET /v1/bundle/requests", e.bundleRequests)

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

// bundleRequests serves GET /v1/bundle/requests
//
// Query params:
//   - from           — YYYY-MM-DD inclusive
//   - to             — YYYY-MM-DD inclusive
//   - from_epoch     — optional catalog epoch, including 0 and -1
//   - to_epoch       — optional catalog epoch, including 0 and -1
//   - status         — optional HTTP status
//   - protocol       — optional path version, such as v1
//   - client_version — optional Zapstore version from the v query
//   - group_by       — comma-separated subset of: day, from_epoch, to_epoch, status, protocol, client_version
func (e *Engine) bundleRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.BundleFilter{
		From:          q.Get("from"),
		To:            q.Get("to"),
		FromEpoch:     q.Get("from_epoch"),
		ToEpoch:       q.Get("to_epoch"),
		Status:        q.Get("status"),
		Protocol:      q.Get("protocol"),
		ClientVersion: q.Get("client_version"),
		GroupBy:       splitCSV(q.Get("group_by")),
	}
	if err := filter.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rows, err := e.store.QueryBundles(ctx, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := make([]bundleResponse, len(rows))
	for i, row := range rows {
		resp[i] = bundleJSON(row, filter.GroupBy)
	}
	writeJSON(w, resp)
}

func bundleJSON(row store.BundleCount, group []string) bundleResponse {
	resp := bundleResponse{Count: row.Count}
	for _, g := range group {
		switch g {
		case "day":
			resp.Day = row.Day
		case "from_epoch":
			v := row.FromEpoch
			resp.FromEpoch = &v
		case "to_epoch":
			v := row.ToEpoch
			resp.ToEpoch = &v
		case "status":
			v := row.Status
			resp.Status = &v
		case "protocol":
			resp.Protocol = row.Protocol
		case "client_version":
			resp.ClientVersion = row.ClientVersion
		}
	}
	return resp
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
