package events

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

// other event kinds that don't have validation functions.
const (
	KindProfile   = 0
	KindDeletion  = 5
	KindForumPost = 11
	KindComment   = 1111
	KindZap       = 9735
)

// WithValidation is a list of event kinds that have validation functions.
var WithValidation = []int{
	KindApp,
	KindRelease,
	KindAsset,
	KindStack,
	KindAppRelays,
	KindIdentityProof,
	KindCommunityCreation,
}

// Validate validates an event by routing to the appropriate
// validation function based on the event kind.
func Validate(event *nostr.Event) error {
	switch event.Kind {
	case KindApp:
		return ValidateApp(event)

	case KindRelease:
		return ValidateRelease(event)

	case KindAsset:
		return ValidateAsset(event)

	case KindStack:
		return ValidateStack(event)

	case KindAppRelays:
		return ValidateAppRelays(event)

	case KindIdentityProof:
		return ValidateIdentityProof(event)

	default:
		return nil
	}
}

// ValidateHash validates a sha256 hash, reporting an error if it is invalid.
func ValidateHash(hash string) error {
	if len(hash) != 64 {
		return fmt.Errorf("invalid sha256 length: %d", len(hash))
	}

	if _, err := hex.DecodeString(hash); err != nil {
		return fmt.Errorf("invalid sha256 hex: %w", err)
	}
	return nil
}

// Find the value of the first tag with the given key.
func Find(tags nostr.Tags, key string) (string, bool) {
	for _, tag := range tags {
		if len(tag) > 1 && tag[0] == key {
			return tag[1], true
		}
	}
	return "", false
}

// AddressableRef is a parsed NIP-01 addressable coordinate in the form "<kind>:<pubkey>:<d-tag>".
type AddressableRef struct {
	Kind   int
	Pubkey string
	DTag   string
}

func (r AddressableRef) String() string {
	return strconv.Itoa(r.Kind) + ":" + r.Pubkey + ":" + r.DTag
}

func (r AddressableRef) Validate() error {
	if r.Kind < 0 || r.Kind > 65535 {
		return fmt.Errorf("invalid kind: %d", r.Kind)
	}
	if r.Pubkey == "" || !nostr.IsValidPublicKey(r.Pubkey) {
		return errors.New("invalid pubkey in addressable ref")
	}
	if r.DTag == "" {
		return errors.New("d-tag must not be empty")
	}
	return nil
}

// ParseAddressableRef parses a string of the form "<kind>:<pubkey>:<d-tag>"
// into an AddressableRef.
func ParseAddressableRef(s string) (AddressableRef, error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return AddressableRef{}, fmt.Errorf("invalid addressable ref %q: must be <kind>:<pubkey>:<d-tag>", s)
	}
	kind, err := strconv.Atoi(parts[0])
	if err != nil {
		return AddressableRef{}, fmt.Errorf("invalid addressable ref %q: kind is not an integer", s)
	}
	return AddressableRef{
		Kind:   kind,
		Pubkey: parts[1],
		DTag:   parts[2],
	}, nil
}
