package catalog

import (
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

// C1Signer is the pubkey that signed the active kind 30509 for the listing's
// certificate. It is not the kind 32267 author: a catalog-key listing has a
// signer only when someone else holds an active proof for that certificate.
// Empty means there is no active proof, and the listing is not a trust target.
func C1Signer(listing Listing, proofs []Proof, at time.Time) string {
	cert, _, ok := listingChain(listing)
	if !ok || cert == "" {
		return ""
	}
	current := map[string]nostr.Event{}
	for _, proof := range proofs {
		if strings.ToLower(proof.D) != cert {
			continue
		}
		event := proof.Event
		if prev, exists := current[event.PubKey]; exists && !newerProof(event, prev) {
			continue
		}
		current[event.PubKey] = event
	}
	var best *nostr.Event
	for _, event := range current {
		if !events.ProofActive(event, at) {
			continue
		}
		if best == nil || newerProof(event, *best) {
			copy := event
			best = &copy
		}
	}
	if best == nil {
		return ""
	}
	return best.PubKey
}

func newerProof(next, current nostr.Event) bool {
	if next.CreatedAt != current.CreatedAt {
		return next.CreatedAt > current.CreatedAt
	}
	return next.ID < current.ID
}
