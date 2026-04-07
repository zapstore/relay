package events

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

const (
	KindProfile   = 0
	KindForumPost = 11
	KindComment   = 1111
	KindZap       = 9735
)

// WithValidation is a list of event kinds that have validation functions.
var WithValidation = []int{
	KindProfile,
	KindForumPost,
	KindApp,
	KindRelease,
	KindAsset,
	KindAppSet,
	KindAppRelays,
	KindIdentityProof,
	KindComment,
	KindZap,
}

// IsZapstoreEvent returns true if the event is a supported Zapstore event type.
func IsZapstoreEvent(e *nostr.Event) bool {
	return slices.Contains(WithValidation, e.Kind)
}

// Validate validates an event by routing to the appropriate
// validation function based on the event kind.
func Validate(event *nostr.Event) error {
	switch event.Kind {
	case KindApp:
		a, ok := Find(event.Tags, "a")
		if ok && strings.HasPrefix(a, "30063:") {
			return fmt.Errorf("legacy app format (a tag referencing 30063) is no longer accepted")
		}
		return ValidateApp(event)

	case KindRelease:
		a, ok := Find(event.Tags, "a")
		if ok && strings.HasPrefix(a, "32267:") {
			return fmt.Errorf("legacy release format (a tag referencing 32267) is no longer accepted")
		}
		return ValidateRelease(event)

	case KindAsset:
		return ValidateAsset(event)

	case KindAppSet:
		return ValidateAppSet(event)

	case KindAppRelays:
		return ValidateAppRelays(event)

	case KindIdentityProof:
		return ValidateIdentityProof(event)

	case KindComment, KindZap:
		return ValidateAppReaction(event)

	default:
		return nil
	}
}

// ValidateAppReaction validates that a zap receipt (9735) is scoped to a kind 32267 app event.
//
// For kind 9735 (NIP-57): the zap receipt carries a lowercase "a" tag set by the LNURL server
// referencing the zapped addressable event.
//
// Kind 1111 comment scoping is handled at the relay layer by CommentScopedToKnownEvent,
// which performs a live DB lookup to allow both root comments and reply threads.
func ValidateAppReaction(event *nostr.Event) error {
	if event.Kind == KindZap {
		// NIP-57: lowercase "a" tag is set by the LNURL server on the zap receipt.
		a, ok := Find(event.Tags, "a")
		if !ok {
			return fmt.Errorf("kind 9735 must have an 'a' tag referencing a kind 32267 app event")
		}
		if err := validateAppAddress(a, "a"); err != nil {
			return fmt.Errorf("kind 9735: %w", err)
		}
	}
	return nil
}

// validateAppAddress checks that an address value is a well-formed "32267:<pubkey>:<d-tag>"
// or "30267:<pubkey>:<d-tag>" string.
func validateAppAddress(addr, tagName string) error {
	if !strings.HasPrefix(addr, "32267:") && !strings.HasPrefix(addr, "30267:") {
		return fmt.Errorf("'%s' tag must reference a kind 32267 app event, got: %s", tagName, addr)
	}
	parts := strings.SplitN(addr, ":", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return fmt.Errorf("'%s' tag must be in the form '32267:<pubkey>:<d-tag>' or '30267:<pubkey>:<d-tag>', got: %s", tagName, addr)
	}
	return nil
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
