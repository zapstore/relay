package events

import (
	"fmt"
	"slices"

	"github.com/nbd-wtf/go-nostr"
)

const KindStack = 30267

// Stack represents a set of app identifiers with associated platform identifiers.
// Learn more here: https://github.com/nostr-protocol/nips/blob/master/51.md#sets
type Stack struct {
	Apps      []AppIdentifier
	Platforms []string
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
			app, err := ParseAppIdentifier(tag[1])
			if err != nil {
				return Stack{}, err
			}
			stack.Apps = append(stack.Apps, app)
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
	stack, err := ParseStack(event)
	if err != nil {
		return err
	}
	return stack.Validate()
}
