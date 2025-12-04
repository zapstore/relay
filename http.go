package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/nbd-wtf/go-nostr/nip19"
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

	// Convert npub to hex format if needed
	var hexPubkey string
	if strings.HasPrefix(pubkey, "npub") {
		_, hex, err := nip19.Decode(pubkey)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(AcceptResponse{
				Accept: false,
			})
			return
		}
		hexPubkey = hex.(string)
	} else {
		hexPubkey = pubkey
	}

	isBlacklisted, err := db.IsBlacklisted(r.Context(), hexPubkey)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(AcceptResponse{
			Accept: false,
		})
		return
	}

	if isBlacklisted {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(AcceptResponse{
			Accept: false,
		})
		return
	}

	isAboveThreshold, err := IsAboveThreshold(hexPubkey)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(AcceptResponse{
			Accept: false,
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(AcceptResponse{
		Accept: isAboveThreshold,
	})
}

func SetupHTTPRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/accept", Accept)
}
