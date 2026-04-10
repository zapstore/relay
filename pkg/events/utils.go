package events

import (
	"encoding/hex"
	"fmt"

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
