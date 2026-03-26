package search

import (
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// TestBuildFilterBy verifies that nostr.Filter fields are correctly translated
// into Typesense filter_by expressions.
func TestBuildFilterBy(t *testing.T) {
	since := nostr.Timestamp(1700000000)
	until := nostr.Timestamp(1800000000)

	tests := []struct {
		name   string
		filter nostr.Filter
		want   string
	}{
		{
			name:   "empty filter",
			filter: nostr.Filter{},
			want:   "",
		},
		{
			name: "single author",
			filter: nostr.Filter{
				Authors: []string{"abc123"},
			},
			want: "pubkey:[abc123]",
		},
		{
			name: "multiple authors",
			filter: nostr.Filter{
				Authors: []string{"abc123", "def456"},
			},
			want: "pubkey:[abc123,def456]",
		},
		{
			name: "since only",
			filter: nostr.Filter{
				Since: &since,
			},
			want: "created_at:>=1700000000",
		},
		{
			name: "until only",
			filter: nostr.Filter{
				Until: &until,
			},
			want: "created_at:<=1800000000",
		},
		{
			name: "since and until",
			filter: nostr.Filter{
				Since: &since,
				Until: &until,
			},
			want: "created_at:>=1700000000 && created_at:<=1800000000",
		},
		{
			name: "t tags",
			filter: nostr.Filter{
				Tags: nostr.TagMap{"t": {"productivity", "tools"}},
			},
			want: "tags:[productivity,tools]",
		},
		{
			name: "f tags are excluded (platform stays in SQLite)",
			filter: nostr.Filter{
				Tags: nostr.TagMap{"f": {"android-arm64-v8a"}},
			},
			want: "",
		},
		{
			name: "authors and since",
			filter: nostr.Filter{
				Authors: []string{"abc123"},
				Since:   &since,
			},
			want: "pubkey:[abc123] && created_at:>=1700000000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildFilterBy(tt.filter)
			if got != tt.want {
				t.Errorf("buildFilterBy() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestEventToDoc verifies that eventToDoc extracts the correct fields.
func TestEventToDoc(t *testing.T) {
	event := &nostr.Event{
		ID:      "abc123",
		Content: "Full description of the app.",
		Tags: nostr.Tags{
			{"d", "com.example.app"},
			{"name", "Example App"},
			{"summary", "A short summary"},
			{"f", "android-arm64-v8a"},
			{"icon", "https://example.com/icon.png"},
		},
	}

	doc := eventToDoc(event)

	if doc.ID != "abc123" {
		t.Errorf("ID = %q, want %q", doc.ID, "abc123")
	}
	if doc.Name != "Example App" {
		t.Errorf("Name = %q, want %q", doc.Name, "Example App")
	}
	if doc.Summary != "A short summary" {
		t.Errorf("Summary = %q, want %q", doc.Summary, "A short summary")
	}
	if doc.Content != "Full description of the app." {
		t.Errorf("Content = %q, want %q", doc.Content, "Full description of the app.")
	}
}

// TestIndexNonBlocking verifies that Index does not block when the channel is full.
func TestIndexNonBlocking(t *testing.T) {
	e := &Engine{
		ops:  make(chan indexOp, 1), // capacity 1
		done: make(chan struct{}),
	}

	event := &nostr.Event{ID: "test1", Kind: 32267}

	// Fill the channel
	e.ops <- indexOp{event: event}

	// This must not block even though the channel is full
	done := make(chan struct{})
	go func() {
		e.Index(event)
		close(done)
	}()

	select {
	case <-done:
		// pass
	case <-time.After(100 * time.Millisecond):
		t.Error("Index() blocked when channel was full")
	}
}

// TestDeleteNonBlocking verifies that Delete does not block when the channel is full.
func TestDeleteNonBlocking(t *testing.T) {
	e := &Engine{
		ops:  make(chan indexOp, 1),
		done: make(chan struct{}),
	}

	e.ops <- indexOp{eventID: "fill"}

	done := make(chan struct{})
	go func() {
		e.Delete("test1")
		close(done)
	}()

	select {
	case <-done:
		// pass
	case <-time.After(100 * time.Millisecond):
		t.Error("Delete() blocked when channel was full")
	}
}

// TestConfigValidate verifies Config.Validate() rules.
func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
		errMsg  string
	}{
		{
			name:    "disabled — no validation",
			cfg:     Config{Enabled: false},
			wantErr: false,
		},
		{
			name:    "enabled without API key",
			cfg:     Config{Enabled: true, URL: "http://localhost:8108"},
			wantErr: true,
			errMsg:  "TYPESENSE_API_KEY",
		},
		{
			name:    "enabled without URL",
			cfg:     Config{Enabled: true, APIKey: "xyz"},
			wantErr: true,
			errMsg:  "TYPESENSE_URL",
		},
		{
			name:    "enabled with all fields",
			cfg:     Config{Enabled: true, URL: "http://localhost:8108", APIKey: "xyz"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("Validate() error = %q, want it to contain %q", err.Error(), tt.errMsg)
			}
		})
	}
}
