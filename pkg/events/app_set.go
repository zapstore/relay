package events

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

const KindAppSet = 30267

type AppIdentifier string // 32267:<pubkey>:<app_id> or 30267:<pubkey>:<d-tag>

// AppSet represents a set of app identifiers with associated platform identifiers.
// Learn more here: https://github.com/nostr-protocol/nips/blob/master/51.md#sets
type AppSet struct {
	Apps      []AppIdentifier
	Platforms []string
}

// Resolve resolves the app set identifiers into a list of public keys and app IDs.
func (s AppSet) Resolve() (pubkeys []string, appIDs []string) {
	for _, app := range s.Apps {
		parts := strings.Split(string(app), ":")
		if len(parts) != 3 {
			continue
		}

		if parts[0] != "32267" {
			continue
		}

		pubkeys = append(pubkeys, parts[1])
		appIDs = append(appIDs, parts[2])
	}
	return pubkeys, appIDs
}

func (s AppSet) Validate() error {
	for _, e := range s.Apps {
		if err := e.Validate(); err != nil {
			return err
		}
	}

	if len(s.Platforms) == 0 {
		return fmt.Errorf("missing 'f' tag (no recognized platform identifier)")
	}
	return nil
}

func (e AppIdentifier) Validate() error {
	parts := strings.Split(string(e), ":")
	if len(parts) != 3 {
		return fmt.Errorf("invalid app set element: %s", e)
	}
	kind, pk, appID := parts[0], parts[1], parts[2]

	if kind != "32267" && kind != "30267" {
		return fmt.Errorf("invalid app set element: %s", e)
	}
	if !nostr.IsValidPublicKey(pk) {
		return errors.New("invalid pubkey in app set element")
	}
	if appID == "" {
		return fmt.Errorf("invalid app ID in app set element: %s", e)
	}
	return nil
}

// ParseAppSet extracts a AppSet from a nostr.Event.
// Returns an error if the event kind is structurally invalid.
func ParseAppSet(event *nostr.Event) (AppSet, error) {
	if event.Kind != KindAppSet {
		return AppSet{}, fmt.Errorf("invalid kind: expected %d, got %d", KindAppSet, event.Kind)
	}

	appSet := AppSet{}
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}

		switch tag[0] {
		case "a":
			appSet.Apps = append(appSet.Apps, AppIdentifier(tag[1]))
		case "f":
			if slices.Contains(PlatformIdentifiers, tag[1]) {
				appSet.Platforms = append(appSet.Platforms, tag[1])
			}
		}
	}
	return appSet, nil
}

// ValidateAppSet parses and validates a AppSet event.
func ValidateAppSet(event *nostr.Event) error {
	appSet, err := ParseAppSet(event)
	if err != nil {
		return err
	}
	return appSet.Validate()
}

// ResolveAppSet resolves the app set identifiers into a list of public keys and app IDs.
// It assumes the app set has already been validated.
func ResolveAppSet(event *nostr.Event) (pubkeys []string, appIDs []string) {
	appSet, err := ParseAppSet(event)
	if err != nil {
		return nil, nil
	}
	return appSet.Resolve()
}
