package events

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

const KindStack = 30267

type AppIdentifier string // 32267:<pubkey>:<app_id>

// const ZapstoreCommunityPubkey = "acfeaea6e51420e8068fac446ca9d17d7a9ef6a5d20d93894e50fee3d4902a84"

// Stack represents a set of app identifiers with associated platform identifiers.
// Learn more here: https://github.com/nostr-protocol/nips/blob/master/51.md#sets
type Stack struct {
	Apps      []AppIdentifier
	Platforms []string
}

// Resolve resolves the app set identifiers into a list of public keys and app IDs.
func (s Stack) Resolve() (pubkeys []string, appIDs []string) {
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

func (s Stack) Validate() error {
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

// ParseStack extracts a Stack from a nostr.Event.
// Returns an error if the event kind is structurally invalid.
func ParseStack(event *nostr.Event) (Stack, error) {
	if event.Kind != KindStack {
		return Stack{}, fmt.Errorf("invalid kind: expected %d, got %d", KindStack, event.Kind)
	}

	stack := Stack{}
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}

		switch tag[0] {
		case "a":
			stack.Apps = append(stack.Apps, AppIdentifier(tag[1]))
		case "f":
			if slices.Contains(PlatformIdentifiers, tag[1]) {
				stack.Platforms = append(stack.Platforms, tag[1])
			}
		}
	}
	return stack, nil
}

// ValidateStack parses and validates a Stack event.
func ValidateStack(event *nostr.Event) error {
	var hVal string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "h" {
			hVal = tag[1]
			break
		}
	}

	isPrivate := event.Content != ""
	if isPrivate && hVal != "" {
		return errors.New("private stacks must not include an 'h' tag")
	}
	// TODO: re-enable once clients are updated to always include h tag on public stacks
	// else {
	// 	if hVal != ZapstoreCommunityPubkey {
	// 		return fmt.Errorf("public stacks must include an 'h' tag with value %s", ZapstoreCommunityPubkey)
	// 	}
	// }

	stack, err := ParseStack(event)
	if err != nil {
		return err
	}
	return stack.Validate()
}

// ResolveStack resolves the stack identifiers into a list of public keys and app IDs.
// It assumes the stack has already been validated.
func ResolveStack(event *nostr.Event) (pubkeys []string, appIDs []string) {
	stack, err := ParseStack(event)
	if err != nil {
		return nil, nil
	}
	return stack.Resolve()
}
