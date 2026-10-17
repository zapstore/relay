package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/zapstore/relay/pkg/relay"
)

func TestModerationTemplate(t *testing.T) {
	d := newTestDashboard(t, Config{}, relay.DB{})
	pub := strings.Repeat("b", 64)
	body := render(t, d, "moderation", moderationPage{
		IsAdmin: true, CanBan: true, Notice: "Banned publisher x",
		Form: banForm{Kind: "nostr", ID: pub, Reason: "Publisher of org.bad", App: "org.bad"},
		Publishers: []banRow{
			{Kind: "forge", ID: "github.com/bad", Reason: "malware", Created: time.Now()},
			{Kind: "nostr", ID: pub, Reason: "spam"},
		},
		Hashes: []hashRow{{Hash: strings.Repeat("a", 64), Reason: "malware"}},
		Now:    time.Now(),
	})
	wantAll(t, body, "github.com/bad", "malware", "npub1", "Lift ban", "Allow", "Banned publisher x",
		`hx-post="/moderation/ban"`, `value="`+pub+`"`, "Publisher of org.bad", `href="/apps/org.bad"`, "Unknown")

	body = render(t, d, "moderation", moderationPage{
		IsAdmin: true, ReadOnly: true,
		Publishers: []banRow{{Kind: "forge", ID: "github.com/bad", Reason: "malware"}},
		Now:        time.Now(),
	})
	wantAll(t, body, "read-only")
	if strings.Contains(body, "hx-post") {
		t.Fatalf("moderation form still offered\n%s", body)
	}
}

func TestHexKey(t *testing.T) {
	hex := strings.Repeat("0", 63) + "1"
	tests := []struct{ in, want string }{
		{npub(hex), hex},
		{hex, hex},
		{"npub1broken", "npub1broken"},
		{"github.com/x", "github.com/x"},
	}
	for _, tt := range tests {
		if got := hexKey(tt.in); got != tt.want {
			t.Errorf("hexKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
