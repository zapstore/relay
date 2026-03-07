package events

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events/legacy"
)

// WithValidation is a list of event kinds that have validation functions.
var WithValidation = []int{
	KindApp,
	KindRelease,
	KindAsset,
	KindAppSet,
	KindAppRelays,
	KindIdentityProof,
	legacy.KindFile,
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
			return legacy.ValidateApp(event)
		}
		return ValidateApp(event)

	case KindRelease:
		a, ok := Find(event.Tags, "a")
		if ok && strings.HasPrefix(a, "32267:") {
			return legacy.ValidateRelease(event)
		}
		return ValidateRelease(event)

	case KindAsset:
		return ValidateAsset(event)

	case legacy.KindFile:
		return legacy.ValidateFile(event)

	case KindAppSet:
		return ValidateAppSet(event)

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
