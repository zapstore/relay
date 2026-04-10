package events

import (
	"fmt"
	"strconv"

	"github.com/nbd-wtf/go-nostr"
)

const KindIdentityProof = 30509

// IdentityProof represents a parsed NIP-C1 Cryptographic Identity Proof event (kind 30509).
type IdentityProof struct {
	// d tag: SHA-256 certificate fingerprint (64 hex chars)
	CertHash string

	// signature tag: base64-encoded signature over the Nostr pubkey
	Signature string

	// expiry tag: unix timestamp after which the proof is no longer valid
	Expiry int64
}

func (p IdentityProof) Validate(event *nostr.Event) error {
	if p.CertHash == "" {
		return fmt.Errorf("missing required 'd' tag (certificate hash)")
	}
	if err := ValidateHash(p.CertHash); err != nil {
		return fmt.Errorf("invalid 'd' tag: %w", err)
	}

	if p.Signature == "" {
		return fmt.Errorf("missing required 'signature' tag")
	}

	if p.Expiry == 0 {
		return fmt.Errorf("missing required 'expiry' tag")
	}
	if p.Expiry <= event.CreatedAt.Time().Unix() {
		return fmt.Errorf("'expiry' must be greater than 'created_at'")
	}
	return nil
}

// ParseIdentityProof extracts an IdentityProof from a nostr.Event.
// Returns an error if the event kind does not match.
func ParseIdentityProof(event *nostr.Event) (IdentityProof, error) {
	if event.Kind != KindIdentityProof {
		return IdentityProof{}, fmt.Errorf("invalid kind: expected %d, got %d", KindIdentityProof, event.Kind)
	}

	proof := IdentityProof{}
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}

		switch tag[0] {
		case "d":
			proof.CertHash = tag[1]
		case "signature":
			proof.Signature = tag[1]
		case "expiry":
			expiry, err := strconv.ParseInt(tag[1], 10, 64)
			if err != nil {
				return IdentityProof{}, fmt.Errorf("invalid 'expiry' tag: %w", err)
			}
			proof.Expiry = expiry
		}
	}
	return proof, nil
}

// ValidateIdentityProof parses and validates a kind 30509 event.
// It checks the event is structurally valid, but doesn't perform signature verification.
func ValidateIdentityProof(event *nostr.Event) error {
	proof, err := ParseIdentityProof(event)
	if err != nil {
		return err
	}
	return proof.Validate(event)
}
