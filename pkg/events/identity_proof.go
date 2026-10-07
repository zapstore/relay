package events

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

const KindIdentityProof = 30509

// IdentityProof is a parsed NIP-C1 kind 30509 event.
type IdentityProof struct {
	CertHash   string
	Signature  string
	Expiry     int64
	Cert       string
	Delegation string
	Revoked    bool
	Reason     string
}

func (p IdentityProof) Validate(event *nostr.Event) error {
	if err := validateProofTags(event); err != nil {
		return err
	}
	if p.Revoked {
		return nil
	}
	if p.Expiry <= event.CreatedAt.Time().Unix() {
		return fmt.Errorf("'expiry' must be greater than 'created_at'")
	}
	if p.Cert == "" {
		return fmt.Errorf("active proof requires a 'cert' tag")
	}
	der, err := base64.StdEncoding.DecodeString(p.Cert)
	if err != nil {
		return fmt.Errorf("invalid 'cert' tag: %w", err)
	}
	sum := sha256.Sum256(der)
	if fmt.Sprintf("%x", sum) != p.CertHash {
		return fmt.Errorf("'cert' hash does not match 'd'")
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("invalid 'cert' tag: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(p.Signature)
	if err != nil {
		return fmt.Errorf("invalid 'signature' tag: %w", err)
	}
	msg := proofMessage(event.CreatedAt.Time().Unix(), p.Expiry, event.PubKey)
	sum = sha256.Sum256([]byte(msg))
	switch pub := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
			return fmt.Errorf("certificate signature: %w", err)
		}
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(pub, sum[:], sig) {
			return fmt.Errorf("certificate signature: verification failed")
		}
	default:
		return fmt.Errorf("unsupported certificate public key")
	}
	return nil
}

func proofMessage(createdAt, expiry int64, pubkey string) string {
	return fmt.Sprintf("Verifying at %d until %d that I control the following Nostr public key: %s", createdAt, expiry, pubkey)
}

func validateProofTags(event *nostr.Event) error {
	var d, sig, exp, cert, del, rev int
	for _, tag := range event.Tags {
		if len(tag) < 1 {
			continue
		}
		switch tag[0] {
		case "d":
			d++
		case "signature":
			sig++
		case "expiry":
			exp++
		case "cert":
			cert++
		case "delegation":
			del++
		case "revoked":
			rev++
		}
	}
	if d != 1 {
		return fmt.Errorf("kind 30509 requires exactly one 'd' tag")
	}
	hash, _ := Find(event.Tags, "d")
	if hash != strings.ToLower(hash) {
		return fmt.Errorf("invalid 'd' tag: must be lowercase hex")
	}
	if err := ValidateHash(hash); err != nil {
		return fmt.Errorf("invalid 'd' tag: %w", err)
	}
	if rev > 1 || sig > 1 || exp > 1 || cert > 1 || del > 1 {
		return fmt.Errorf("kind 30509 has a duplicate tag")
	}
	if rev == 1 {
		if sig != 0 || exp != 0 || cert != 0 || del != 0 {
			return fmt.Errorf("revocation must omit signature, expiry, cert, and delegation")
		}
		return nil
	}
	if sig != 1 || exp != 1 {
		return fmt.Errorf("active proof requires 'signature' and 'expiry'")
	}
	rawExp, _ := Find(event.Tags, "expiry")
	expiry, err := strconv.ParseInt(rawExp, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid 'expiry' tag: %w", err)
	}
	if expiry == 0 {
		return fmt.Errorf("missing required 'expiry' tag")
	}
	if del == 1 {
		delegate, _ := Find(event.Tags, "delegation")
		if delegate != strings.ToLower(delegate) {
			return fmt.Errorf("invalid 'delegation' tag")
		}
		if err := ValidateHash(delegate); err != nil {
			return fmt.Errorf("invalid 'delegation' tag: %w", err)
		}
	}
	return nil
}

// ParseIdentityProof extracts an IdentityProof from a nostr.Event.
func ParseIdentityProof(event *nostr.Event) (IdentityProof, error) {
	if event.Kind != KindIdentityProof {
		return IdentityProof{}, fmt.Errorf("invalid kind: expected %d, got %d", KindIdentityProof, event.Kind)
	}
	proof := IdentityProof{}
	for _, tag := range event.Tags {
		if len(tag) < 2 && (len(tag) < 1 || tag[0] != "revoked") {
			continue
		}
		if len(tag) < 1 {
			continue
		}
		switch tag[0] {
		case "d":
			if len(tag) > 1 {
				proof.CertHash = tag[1]
			}
		case "signature":
			if len(tag) > 1 {
				proof.Signature = tag[1]
			}
		case "expiry":
			if len(tag) < 2 {
				continue
			}
			expiry, err := strconv.ParseInt(tag[1], 10, 64)
			if err != nil {
				return IdentityProof{}, fmt.Errorf("invalid 'expiry' tag: %w", err)
			}
			proof.Expiry = expiry
		case "cert":
			if len(tag) > 1 {
				proof.Cert = tag[1]
			}
		case "delegation":
			if len(tag) > 1 {
				proof.Delegation = tag[1]
			}
		case "revoked":
			proof.Revoked = true
			if len(tag) > 1 {
				proof.Reason = tag[1]
			}
		}
	}
	return proof, nil
}

// ValidateIdentityProof parses a kind 30509 and verifies it under NIP-C1.
// An active proof must carry its certificate. A revocation omits the certificate signature.
// Expiry is not judged here; the snapshot decides whether the proof is still active.
func ValidateIdentityProof(event *nostr.Event) error {
	proof, err := ParseIdentityProof(event)
	if err != nil {
		return err
	}
	return proof.Validate(event)
}

// ProofActive reports whether the current kind 30509 is an active proof at at.
// A revocation is inactive. Expiry at or before at is inactive.
func ProofActive(event nostr.Event, at time.Time) bool {
	if _, ok := Find(event.Tags, "revoked"); ok || hasTag(event.Tags, "revoked") {
		return false
	}
	raw, ok := Find(event.Tags, "expiry")
	if !ok {
		return false
	}
	expiry, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return false
	}
	return expiry > at.Unix()
}

func hasTag(tags nostr.Tags, key string) bool {
	for _, tag := range tags {
		if len(tag) > 0 && tag[0] == key {
			return true
		}
	}
	return false
}
