package catalog

import (
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestC1SignerIsTheProofNotTheListingAuthor(t *testing.T) {
	catalogKey := strings.Repeat("11", 32)
	owner := strings.Repeat("22", 32)
	cert := strings.Repeat("ab", 32)
	listing := Listing{
		AppID: "com.example",
		App:   nostr.Event{PubKey: catalogKey},
		Assets: []nostr.Event{{
			Tags: nostr.Tags{{"apk_certificate_hash", cert}},
		}},
	}
	at := time.Unix(200, 0)
	proof := nostr.Event{
		PubKey:    owner,
		Kind:      events.KindIdentityProof,
		CreatedAt: 100,
		Tags:      nostr.Tags{{"d", cert}, {"expiry", "9999999999"}},
	}
	if got := C1Signer(listing, []Proof{{Pubkey: owner, D: cert, Event: proof}}, at); got != owner {
		t.Fatalf("signer = %s", got)
	}
	if got := C1Signer(listing, nil, at); got != "" {
		t.Fatalf("no proof: %s", got)
	}
	revoked := proof
	revoked.Tags = nostr.Tags{{"d", cert}, {"revoked"}}
	if got := C1Signer(listing, []Proof{{Pubkey: owner, D: cert, Event: revoked}}, at); got != "" {
		t.Fatalf("revoked: %s", got)
	}
	other := strings.Repeat("cd", 32)
	if got := C1Signer(listing, []Proof{{Pubkey: owner, D: other, Event: proof}}, at); got != "" {
		t.Fatalf("other certificate: %s", got)
	}
}
