package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nbd-wtf/go-nostr/nip19"
)

type AcceptResponse struct {
	Accept bool `json:"accept"`
}

type ProcessRequest struct {
	ApkURL      string `json:"apkUrl"`
	Pubkey      string `json:"pubkey"`
	Repository  string `json:"repository,omitempty"`
	Description string `json:"description,omitempty"`
	License     string `json:"license,omitempty"`
}

type ProcessResponse struct {
	Events []json.RawMessage `json:"events"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// corsMiddleware adds CORS headers for zapstore.dev
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "https://zapstore.dev" || origin == "http://localhost:5173" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}
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

// checkPubkeyAccepted checks if a pubkey is accepted (not blacklisted and above WoT threshold)
func checkPubkeyAccepted(ctx context.Context, pubkey string) (bool, error) {
	// Convert npub to hex format if needed
	var hexPubkey string
	if strings.HasPrefix(pubkey, "npub") {
		_, hex, err := nip19.Decode(pubkey)
		if err != nil {
			return false, fmt.Errorf("invalid npub format")
		}
		hexPubkey = hex.(string)
	} else {
		hexPubkey = pubkey
	}

	isBlacklisted, err := db.IsBlacklisted(ctx, hexPubkey)
	if err != nil {
		return false, err
	}

	if isBlacklisted {
		return false, nil
	}

	isAboveThreshold, err := IsAboveThreshold(hexPubkey)
	if err != nil {
		return false, err
	}

	return isAboveThreshold, nil
}

// downloadFile downloads a file from URL to the specified path
func downloadFile(url, destPath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status: %d", resp.StatusCode)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

func Process(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(ErrorResponse{Error: "method not allowed"})
		return
	}

	var req ProcessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ErrorResponse{Error: "invalid request body"})
		return
	}

	if req.ApkURL == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ErrorResponse{Error: "apkUrl is required"})
		return
	}

	if req.Pubkey == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ErrorResponse{Error: "pubkey is required"})
		return
	}

	// Check pubkey against accept logic
	accepted, err := checkPubkeyAccepted(r.Context(), req.Pubkey)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(ErrorResponse{Error: "failed to verify pubkey"})
		return
	}

	if !accepted {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(ErrorResponse{Error: "pubkey not accepted"})
		return
	}

	// Download APK to /tmp
	apkFilename := filepath.Base(req.ApkURL)
	if apkFilename == "" || apkFilename == "." {
		apkFilename = "downloaded.apk"
	}
	apkPath := filepath.Join("/tmp", apkFilename)

	if err := downloadFile(req.ApkURL, apkPath); err != nil {
		log.Printf("Failed to download APK: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ErrorResponse{Error: "failed to download APK"})
		return
	}
	defer os.Remove(apkPath) // Clean up after processing

	// Build YAML config for zapstore publish
	var yamlBuilder strings.Builder
	yamlBuilder.WriteString(fmt.Sprintf("assets: [%s]\n", apkPath))

	// If any optional fields are present, include them (new app)
	// If none are present, it's an update
	if req.Description != "" {
		yamlBuilder.WriteString(fmt.Sprintf("description: %s\n", req.Description))
	}
	if req.Repository != "" {
		yamlBuilder.WriteString(fmt.Sprintf("repository: %s\n", req.Repository))
	}
	if req.License != "" {
		yamlBuilder.WriteString(fmt.Sprintf("license: %s\n", req.License))
	}

	yamlConfig := yamlBuilder.String()
	log.Printf("Running zapstore publish with config:\n%s", yamlConfig)

	// Call zapstore publish with YAML via stdin
	cmd := exec.Command("zapstore", "publish")
	cmd.Env = append(os.Environ(), fmt.Sprintf("SIGN_WITH=%s", req.Pubkey))
	cmd.Stdin = strings.NewReader(yamlConfig)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		log.Printf("zapstore publish failed: %v, stderr: %s", err, stderr.String())
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(ErrorResponse{Error: fmt.Sprintf("zapstore publish failed: %s", stderr.String())})
		return
	}

	// Parse the output - expecting JSON events
	output := stdout.String()
	log.Printf("zapstore publish output: %s", output)

	// Parse the output as JSON array of events
	var events []json.RawMessage
	if err := json.Unmarshal([]byte(output), &events); err != nil {
		// Try to parse as newline-delimited JSON
		lines := strings.Split(strings.TrimSpace(output), "\n")
		events = make([]json.RawMessage, 0, len(lines))
		for _, line := range lines {
			if line != "" {
				events = append(events, json.RawMessage(line))
			}
		}
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(ProcessResponse{Events: events})
}

func SetupHTTPRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/accept", corsMiddleware(Accept))
	mux.HandleFunc("/api/v1/process", corsMiddleware(Process))
}
