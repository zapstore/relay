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

// ValidateIdentityProof validates the structure of a kind 30509 event.
func ValidateIdentityProof(event *nostr.Event) error {
	if event.Kind != KindIdentityProof {
		return fmt.Errorf("invalid kind: expected %d, got %d", KindIdentityProof, event.Kind)
	}

	certHash, ok := Find(event.Tags, "d")
	if !ok || certHash == "" {
		return fmt.Errorf("missing required 'd' tag (certificate hash)")
	}
	if err := ValidateHash(certHash); err != nil {
		return fmt.Errorf("invalid 'd' tag: %w", err)
	}

	sig, ok := Find(event.Tags, "signature")
	if !ok || sig == "" {
		return fmt.Errorf("missing required 'signature' tag")
	}

	expiryStr, ok := Find(event.Tags, "expiry")
	if !ok || expiryStr == "" {
		return fmt.Errorf("missing required 'expiry' tag")
	}
	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid 'expiry' tag: %w", err)
	}
	if expiry <= event.CreatedAt.Time().Unix() {
		return fmt.Errorf("'expiry' must be greater than 'created_at'")
	}

	return nil
}

// ParseIdentityProof parses a kind 30509 event into an IdentityProof.
func ParseIdentityProof(event *nostr.Event) (IdentityProof, error) {
	if err := ValidateIdentityProof(event); err != nil {
		return IdentityProof{}, err
	}

	certHash, _ := Find(event.Tags, "d")
	sig, _ := Find(event.Tags, "signature")
	expiryStr, _ := Find(event.Tags, "expiry")
	expiry, _ := strconv.ParseInt(expiryStr, 10, 64)

	return IdentityProof{
		CertHash:  certHash,
		Signature: sig,
		Expiry:    expiry,
	}, nil
}
