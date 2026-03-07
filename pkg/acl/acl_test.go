package acl

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestManualHotReload is a manual test for hot-reloading. Run with:
//
// go test -run TestManualHotReload -v
//
// Then edit files in pkg/acl/testdata/ to see reload logs.
func TestManualHotReload(t *testing.T) {
	// t.Skip("skipping manual test")

	dir := "testdata"
	defer os.RemoveAll(dir)

	config := Config{UnknownPubkeyPolicy: AllowAll}

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)

	controller, err := New(config, dir, logger)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}
	defer controller.Close()

	t.Logf("watching %s for 30 seconds...", dir)
	t.Logf("allowed_pubkeys: %d", controller.pubkeysAllowed.Size())
	t.Logf("blocked_pubkeys: %d", controller.pubkeysBlocked.Size())
	t.Logf("platform_users_blocked: %d", controller.platformUsersBlocked.Size())
	time.Sleep(30 * time.Second)
}

func TestToPubkey(t *testing.T) {
	tests := []struct {
		name     string
		pubkey   string
		expected string
	}{
		{
			pubkey:   "726a1e261cc6474674e8285e3951b3bb139be9a773d1acf49dc868db861a1c11",
			expected: "726a1e261cc6474674e8285e3951b3bb139be9a773d1acf49dc868db861a1c11",
		},
		{
			pubkey:   "npub1wf4pufsucer5va8g9p0rj5dnhvfeh6d8w0g6eayaep5dhps6rsgs43dgh9",
			expected: "726a1e261cc6474674e8285e3951b3bb139be9a773d1acf49dc868db861a1c11",
		},
		{
			pubkey:   "invalid",
			expected: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pubkey, _ := toPubkey(test.pubkey)
			if pubkey != test.expected {
				t.Errorf("toPubkey() expected %s, got %s", test.expected, pubkey)
			}
		})
	}
}

func TestParseCSV(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		entities []string
		reasons  []string
	}{
		{
			name:    "empty file",
			content: "",
		},
		{
			name:    "only comments",
			content: "# comment 1\n# comment 2",
		},
		{
			name:     "valid",
			content:  "abc,some reason\ndef,another reason",
			entities: []string{"abc", "def"},
			reasons:  []string{"some reason", "another reason"},
		},
		{
			name:     "valid with empty line",
			content:  "abc,some reason\n\ndef,another reason",
			entities: []string{"abc", "def"},
			reasons:  []string{"some reason", "another reason"},
		},
		{
			name:     "valid with comment",
			content:  "abc,some reason\n# comment\ndef,another reason",
			entities: []string{"abc", "def"},
			reasons:  []string{"some reason", "another reason"},
		},
		{
			name:     "valid with quotes",
			content:  "abc,\"some reason with comma\"\ndef,another reason",
			entities: []string{"abc", "def"},
			reasons:  []string{"some reason with comma", "another reason"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeTempFile(t, test.content)

			entities, reasons, err := parseCSV(path)
			if err != nil {
				t.Fatalf("parseCSV() error = %v", err)
			}

			if !slices.Equal(entities, test.entities) {
				t.Errorf("parseCSV() expected entities %v, got %v", test.entities, entities)
			}
			if !slices.Equal(reasons, test.reasons) {
				t.Errorf("parseCSV() expected reasons %v, got %v", test.reasons, reasons)
			}
		})
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.csv")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	return path
}
