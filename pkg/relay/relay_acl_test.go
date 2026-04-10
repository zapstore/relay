package relay

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/rely"
	"github.com/zapstore/relay/pkg/acl"
	"github.com/zapstore/relay/pkg/events"
)

// TestAuthorNotAllowed_baseVsOpenKinds ensures kinds that use AllowEvent are rejected under BlockAll
// for unknown authors, while open kinds (blocklist-only) still pass.
func TestAuthorNotAllowed_baseVsOpenKinds(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	cfg := acl.Config{UnknownPubkeyPolicy: acl.BlockAll}
	c, err := acl.New(cfg, t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	unknownPub := strings.Repeat("b", 64)
	h := AuthorNotAllowed(c, "")

	tests := []struct {
		name      string
		kind      int
		wantBlock bool
	}{
		{"kind_32267_app", events.KindApp, true},
		{"kind_30267_stack", events.KindStack, false},
		{"kind_11_forum", events.KindForumPost, true},
		{"kind_1111_comment", events.KindComment, false},
		{"kind_9735_zap", events.KindZap, false},
		{"kind_30509_identity", events.KindIdentityProof, false},
	}

	var noClient rely.Client
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &nostr.Event{Kind: tt.kind, PubKey: unknownPub}
			err := h(noClient, e)
			if tt.wantBlock {
				if err != ErrEventPubkeyBlocked {
					t.Fatalf("want ErrEventPubkeyBlocked, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("want nil err, got %v", err)
			}
		})
	}
}

func TestAuthorNotAllowed_derivedKindRespectsBlocklist(t *testing.T) {
	dir := t.TempDir()
	blockedPub := strings.Repeat("c", 64)
	files := map[string]string{
		acl.PubkeysAllowedFile:       "# Allowed pubkeys\n# pubkey,reason\n",
		acl.PubkeysBlockedFile:       "# Blocked pubkeys\n# pubkey,reason\n" + blockedPub + ",test\n",
		acl.PlatformUsersBlockedFile: "# Blocked platform users\n# platform:username,reason\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}

	logger := slog.New(slog.DiscardHandler)
	cfg := acl.Config{UnknownPubkeyPolicy: acl.AllowAll}
	c, err := acl.New(cfg, dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	h := AuthorNotAllowed(c, "")
	var noClient rely.Client
	e := &nostr.Event{Kind: events.KindComment, PubKey: blockedPub}
	if err := h(noClient, e); err != ErrEventPubkeyBlocked {
		t.Fatalf("blocked pubkey on kind 1111: want ErrEventPubkeyBlocked, got %v", err)
	}
}
