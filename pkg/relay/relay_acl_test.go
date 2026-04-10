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

// The relay enforces three ACL strategies on incoming events:
//
//   - Pessimist: full ACL check (allowlist + blocklist + Vertex).
//     Kinds: 32267 (App), 30063 (Release), 3063 (Asset), 10222 (Community), 30509 (IdentityProof)
//
//   - Optimist: blocklist-only; Sentinel scans in background.
//     Kinds: 30267 (Stack), 11 (ForumPost)
//
//   - Optimist+anchor: blocklist-only, but the event must be anchored to an
//     existing relay event (enforced separately by NotAnchored).
//     Kinds: 0 (Profile), 5 (Deletion), 1111 (Comment), 9735 (Zap)

// TestAuthorNotAllowed_Pessimist verifies that Pessimist kinds block an unknown pubkey
// when the relay policy is BlockAll, i.e. the full ACL check rejects authors not on
// the allowlist.
func TestAuthorNotAllowed_Pessimist(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	cfg := acl.Config{UnknownPubkeyPolicy: acl.BlockAll}
	c, err := acl.New(cfg, t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	unknownPub := strings.Repeat("b", 64)
	h := AuthorNotAllowed(c, "")
	var noClient rely.Client

	kinds := []struct {
		name string
		kind int
	}{
		{"app_32267", events.KindApp},
		{"release_30063", events.KindRelease},
		{"asset_3063", events.KindAsset},
		{"community_10222", events.KindCommunityCreation},
		{"identity_proof_30509", events.KindIdentityProof},
	}

	for _, tt := range kinds {
		t.Run(tt.name, func(t *testing.T) {
			e := &nostr.Event{Kind: tt.kind, PubKey: unknownPub}
			if err := h(noClient, e); err != ErrEventPubkeyBlocked {
				t.Fatalf("kind %d: want ErrEventPubkeyBlocked, got %v", tt.kind, err)
			}
		})
	}
}

// TestAuthorNotAllowed_Optimist verifies that Optimist kinds (non-app root events) allow
// unknown pubkeys through — only explicitly blocked pubkeys are rejected.
// The BlockAll policy must not affect these kinds.
func TestAuthorNotAllowed_Optimist(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	cfg := acl.Config{UnknownPubkeyPolicy: acl.BlockAll}
	c, err := acl.New(cfg, t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	unknownPub := strings.Repeat("b", 64)
	h := AuthorNotAllowed(c, "")
	var noClient rely.Client

	kinds := []struct {
		name string
		kind int
	}{
		{"stack_30267", events.KindStack},
		{"forum_post_11", events.KindForumPost},
	}

	for _, tt := range kinds {
		t.Run(tt.name, func(t *testing.T) {
			e := &nostr.Event{Kind: tt.kind, PubKey: unknownPub}
			if err := h(noClient, e); err != nil {
				t.Fatalf("kind %d: want nil, got %v", tt.kind, err)
			}
		})
	}
}

// TestAuthorNotAllowed_OptimistAnchor verifies that Optimist+anchor kinds allow unknown
// pubkeys through — anchoring is enforced separately by NotAnchored, not here.
// The BlockAll policy must not affect these kinds.
func TestAuthorNotAllowed_OptimistAnchor(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	cfg := acl.Config{UnknownPubkeyPolicy: acl.BlockAll}
	c, err := acl.New(cfg, t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	unknownPub := strings.Repeat("b", 64)
	h := AuthorNotAllowed(c, "")
	var noClient rely.Client

	kinds := []struct {
		name string
		kind int
	}{
		{"profile_0", events.KindProfile},
		{"deletion_5", events.KindDeletion},
		{"comment_1111", events.KindComment},
		{"zap_9735", events.KindZap},
	}

	for _, tt := range kinds {
		t.Run(tt.name, func(t *testing.T) {
			e := &nostr.Event{Kind: tt.kind, PubKey: unknownPub}
			if err := h(noClient, e); err != nil {
				t.Fatalf("kind %d: want nil, got %v", tt.kind, err)
			}
		})
	}
}

// TestAuthorNotAllowed_BlocklistRejectsAll verifies that a blocked pubkey is rejected
// across all three strategies — the blocklist is a hard floor regardless of kind.
func TestAuthorNotAllowed_BlocklistRejectsAll(t *testing.T) {
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

	kinds := []struct {
		name string
		kind int
	}{
		// Pessimist
		{"app_32267", events.KindApp},
		{"release_30063", events.KindRelease},
		{"asset_3063", events.KindAsset},
		{"community_10222", events.KindCommunityCreation},
		{"identity_proof_30509", events.KindIdentityProof},
		// Optimist
		{"stack_30267", events.KindStack},
		{"forum_post_11", events.KindForumPost},
		// Optimist+anchor
		{"profile_0", events.KindProfile},
		{"deletion_5", events.KindDeletion},
		{"comment_1111", events.KindComment},
		{"zap_9735", events.KindZap},
	}

	for _, tt := range kinds {
		t.Run(tt.name, func(t *testing.T) {
			e := &nostr.Event{Kind: tt.kind, PubKey: blockedPub}
			if err := h(noClient, e); err != ErrEventPubkeyBlocked {
				t.Fatalf("blocked pubkey on kind %d: want ErrEventPubkeyBlocked, got %v", tt.kind, err)
			}
		})
	}
}
