package main

import (
	"encoding/json"
	"net/http"
)

// Response structures for JSON responses
type SuccessResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data"`
}

type ErrorResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

type BlacklistResponse struct {
	Pubkey        string `json:"pubkey"`
	IsBlacklisted bool   `json:"is_blacklisted"`
}

type WoTRankResponse struct {
	Pubkey string  `json:"pubkey"`
	Rank   float64 `json:"rank"`
}

func IsBlacklistHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	pubkey := r.URL.Query().Get("pubkey")
	if pubkey == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ErrorResponse{
			Success: false,
			Error:   "pubkey parameter is required",
		})
		return
	}

	isBlacklisted, err := db.IsBlacklisted(r.Context(), pubkey)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(ErrorResponse{
			Success: false,
			Error:   "failed to check blacklist status: " + err.Error(),
		})
		return
	}

	response := SuccessResponse{
		Success: true,
		Data: BlacklistResponse{
			Pubkey:        pubkey,
			IsBlacklisted: isBlacklisted,
		},
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

func WoTRankHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	pubkey := r.URL.Query().Get("pubkey")
	if pubkey == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ErrorResponse{
			Success: false,
			Error:   "pubkey parameter is required",
		})
		return
	}

	rank, err := GetWoTRank(pubkey)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(ErrorResponse{
			Success: false,
			Error:   "failed to get WoT rank: " + err.Error(),
		})
		return
	}

	response := SuccessResponse{
		Success: true,
		Data: WoTRankResponse{
			Pubkey: pubkey,
			Rank:   rank,
		},
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

func SetupHTTPRoutes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/blacklist", IsBlacklistHandler)
	mux.HandleFunc("/api/v1/wot-rank", WoTRankHandler)

	return mux
}
