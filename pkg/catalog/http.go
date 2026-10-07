package catalog

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Handler serves GET /bundle from snapshots and cached bundles.
type Handler struct {
	Data   string
	Signer Signer
	Now    func() time.Time
	Log    *slog.Logger
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET is required", http.StatusMethodNotAllowed)
		return
	}
	fromRaw := strings.TrimSpace(r.URL.Query().Get("from"))
	if fromRaw == "" {
		http.Error(w, "from is required", http.StatusBadRequest)
		return
	}
	from, err := strconv.ParseInt(fromRaw, 10, 64)
	if err != nil || from < 0 || strconv.FormatInt(from, 10) != fromRaw {
		http.Error(w, "from must be a non-negative integer", http.StatusBadRequest)
		return
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("v")); raw != "" && raw != "1" {
		http.Error(w, "unsupported catalog-sync version", http.StatusBadRequest)
		return
	}
	to, err := LatestSnap(h.Data)
	if err != nil {
		h.logErr(err)
		http.Error(w, "failed to read catalog epoch", http.StatusInternalServerError)
		return
	}
	if from > to {
		http.Error(w, "from is newer than the current catalog epoch", http.StatusBadRequest)
		return
	}
	if from == to {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	body, err := EnsureBundle(r.Context(), h.Data, from, to, h.Signer, now.Unix())
	if err != nil {
		h.logErr(err)
		http.Error(w, "failed to build catalog update", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", MediaType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		h.logErr(err)
	}
}

func (h Handler) logErr(err error) {
	if h.Log != nil && err != nil {
		h.Log.Error("bundle", "error", err)
	}
}
