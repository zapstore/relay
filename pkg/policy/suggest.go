package policy

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/pippellia-btc/rely/v2"
	"github.com/zapstore/relay/pkg/rate"
)

type suggestRequest struct {
	Repository      string `json:"repository"`
	ReleaseSource   string `json:"release_source"`
	CertificateHash string `json:"certificate_hash"`
}

// Suggest handles POST /suggest.
// A handled request is 201 with an empty body, including a bad URL, which is discarded.
// A storage failure is 500.
func Suggest(store *Store, limiter rate.Limiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !limiter.Allow(rely.GetIP(r).Group(), 1) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		body := http.MaxBytesReader(w, r.Body, 1<<16)
		defer body.Close()
		var req suggestRequest
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusCreated)
			return
		}
		if _, _, err := store.PutSuggestion(r.Context(), req.Repository, req.ReleaseSource, req.CertificateHash); err != nil {
			if errors.Is(err, ErrSuggest) || errors.Is(err, ErrPublisher) {
				w.WriteHeader(http.StatusCreated)
				return
			}
			slog.Error("suggest", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}
}
