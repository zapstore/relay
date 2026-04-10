package events

import (
	"fmt"

	"github.com/nbd-wtf/go-nostr"
)

const KindRelease = 30063

// Release represents a parsed Software Release event (kind 30063).
// Learn more here: https://github.com/franzaps/nips/blob/applications/82.md
type Release struct {
	// Required fields
	I        string   // App identifier (must match 'd' tag from application)
	Version  string   // Release version (e.g., "1.2.3", "v0.1.2-rc1")
	D        string   // Identifier@version
	Channel  string   // Channel ID
	AssetIDs []string // Asset event IDs (at least one required)

	// Content
	Content string // Full release notes, markdown allowed
}

// Validate checks that all required fields are present and valid.
func (r Release) Validate() error {
	if r.I == "" {
		return fmt.Errorf("missing or empty 'i' tag (app identifier)")
	}
	if r.Version == "" {
		return fmt.Errorf("missing or empty 'version' tag")
	}
	if r.D == "" {
		return fmt.Errorf("missing or empty 'd' tag")
	}
	if r.Channel == "" {
		return fmt.Errorf("missing or empty 'c' tag (channel ID)")
	}
	if len(r.AssetIDs) == 0 {
		return fmt.Errorf("missing 'e' tag (asset ID)")
	}
	for i, id := range r.AssetIDs {
		if err := ValidateHash(id); err != nil {
			return fmt.Errorf("invalid 'e' tag at index %d: %w", i, err)
		}
	}

	// Validate that 'd' tag equals 'i' + '@' + 'version'
	expectedD := r.I + "@" + r.Version
	if r.D != expectedD {
		return fmt.Errorf("invalid 'd' tag: expected '%s', got '%s'", expectedD, r.D)
	}
	return nil
}

// ParseRelease extracts a Release from a nostr.Event.
// Returns an error if the event kind is wrong or if duplicate singular tags are found.
func ParseRelease(event *nostr.Event) (Release, error) {
	if event.Kind != KindRelease {
		return Release{}, fmt.Errorf("invalid kind: expected %d, got %d", KindRelease, event.Kind)
	}

	release := Release{Content: event.Content}

	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}

		switch tag[0] {
		case "i":
			if release.I != "" {
				return Release{}, fmt.Errorf("duplicate 'i' tag")
			}
			release.I = tag[1]

		case "version":
			if release.Version != "" {
				return Release{}, fmt.Errorf("duplicate 'version' tag")
			}
			release.Version = tag[1]

		case "d":
			if release.D != "" {
				return Release{}, fmt.Errorf("duplicate 'd' tag")
			}
			release.D = tag[1]

		case "c":
			if release.Channel != "" {
				return Release{}, fmt.Errorf("duplicate 'c' tag")
			}
			release.Channel = tag[1]

		case "e":
			release.AssetIDs = append(release.AssetIDs, tag[1])
		}
	}
	return release, nil
}

// ValidateRelease parses and validates a Software Release event.
func ValidateRelease(event *nostr.Event) error {
	release, err := ParseRelease(event)
	if err != nil {
		return err
	}
	return release.Validate()
}
