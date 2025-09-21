package main

import (
	"encoding/json"
	"net/http"
)

type AcceptResponse struct {
	Accept bool `json:"accept"`
}

func Accept(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	pubkey := r.URL.Query().Get("pubkey")
	if pubkey == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(AcceptResponse{
			Accept: false,
		})
		return
	}

	isBlacklisted, err := db.IsBlacklisted(r.Context(), pubkey)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(AcceptResponse{
			Accept: false,
		})
		return
	}

	isAboveThreshold, err := IsAboveThreshold(pubkey)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(AcceptResponse{
			Accept: false,
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(AcceptResponse{
		Accept: isAboveThreshold && !isBlacklisted,
	})
}

func SetupHTTPRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/accept", Accept)

	return mux
}
