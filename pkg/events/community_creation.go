package events

import (
	"fmt"
	"strconv"

	"github.com/nbd-wtf/go-nostr"
)

const KindCommunityCreation = 10222

// ContentSection represents one named content section within a community,
// grouping a name, the event kinds it accepts, the profile-list addresses
// that whitelist publishers, and the badge definitions that grant publish rights.
type ContentSection struct {
	// Name is the value of the "content" tag that opened this section.
	Name string

	// Kinds holds the event kinds accepted in this section (from "k" tags).
	Kinds []int

	// Lists holds addressable references to kind-30000 profile lists
	// that whitelist publishers.
	Lists []AddressableRef

	// Badges holds badge definition references (format: "30009:<pubkey>:<d-tag>")
	// that grant publish rights. Users holding any listed badge may publish.
	Badges []AddressableRef
}

// CommunityCreation represents a parsed kind-10222 Community Creation event.
type CommunityCreation struct {
	// Relays holds the relay URLs for community content. First entry is the main relay.
	Relays []string

	// Blossoms holds optional blossom server URLs.
	Blossoms []string

	// Mints holds optional ecash mint URLs.
	Mints []string

	// Sections holds the ordered content sections defined for this community.
	Sections []ContentSection

	// ToS is an optional reference to the community's terms of service event.
	ToS string

	// Location is an optional human-readable location string.
	Location string

	// GeoHash is an optional NIP-52 geo hash.
	GeoHash string

	// Description optionally overrides the pubkey's kind-0 description.
	Description string
}

func (s ContentSection) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("content section has an empty name")
	}
	if len(s.Kinds) == 0 {
		return fmt.Errorf("content section %q has no 'k' tags", s.Name)
	}
	for _, list := range s.Lists {
		if list.Kind != 30000 {
			return fmt.Errorf("invalid list ref %q: expected kind 30000, got %d", list, list.Kind)
		}
		if err := list.Validate(); err != nil {
			return fmt.Errorf("invalid list ref %q: %w", list, err)
		}
	}
	for _, badge := range s.Badges {
		if badge.Kind != 30009 {
			return fmt.Errorf("invalid badge ref %q: expected kind 30009, got %d", badge, badge.Kind)
		}
		if err := badge.Validate(); err != nil {
			return fmt.Errorf("invalid badge ref %q: %w", badge, err)
		}
	}
	return nil
}

func (c CommunityCreation) Validate() error {
	if len(c.Relays) == 0 {
		return fmt.Errorf("missing required 'r' tag (at least one relay URL)")
	}
	for _, r := range c.Relays {
		if !nostr.IsValidRelayURL(r) {
			return fmt.Errorf("invalid relay URL: %s", r)
		}
	}

	if len(c.Sections) == 0 {
		return fmt.Errorf("missing required 'content' tag (at least one content section)")
	}
	for _, s := range c.Sections {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("content section %q: %w", s.Name, err)
		}
	}
	return nil
}

// ParseCommunityCreation extracts a CommunityCreation from a nostr.Event.
// Returns an error if the event kind does not match.
func ParseCommunityCreation(event *nostr.Event) (CommunityCreation, error) {
	if event.Kind != KindCommunityCreation {
		return CommunityCreation{}, fmt.Errorf("invalid kind: expected %d, got %d", KindCommunityCreation, event.Kind)
	}

	community := CommunityCreation{}
	var current *ContentSection

	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}

		switch tag[0] {
		case "r":
			community.Relays = append(community.Relays, tag[1])

		case "blossom":
			community.Blossoms = append(community.Blossoms, tag[1])

		case "mint":
			community.Mints = append(community.Mints, tag[1])

		case "content":
			// Each "content" tag opens a new section; flush the previous one.
			if current != nil {
				community.Sections = append(community.Sections, *current)
			}
			current = &ContentSection{Name: tag[1]}

		case "k":
			if current != nil {
				if k, err := strconv.Atoi(tag[1]); err == nil {
					current.Kinds = append(current.Kinds, k)
				}
			}

		case "a":
			if current != nil {
				if ref, err := ParseAddressableRef(tag[1]); err == nil {
					current.Lists = append(current.Lists, ref)
				}
			}

		case "badge":
			if current != nil {
				if ref, err := ParseAddressableRef(tag[1]); err == nil {
					current.Badges = append(current.Badges, ref)
				}
			}

		case "tos":
			community.ToS = tag[1]

		case "location":
			community.Location = tag[1]

		case "g":
			community.GeoHash = tag[1]

		case "description":
			community.Description = tag[1]
		}
	}

	// Flush the last open section.
	if current != nil {
		community.Sections = append(community.Sections, *current)
	}
	return community, nil
}

// ValidateCommunityCreation parses and validates a kind-10222 event.
func ValidateCommunityCreation(event *nostr.Event) error {
	community, err := ParseCommunityCreation(event)
	if err != nil {
		return err
	}
	return community.Validate()
}
