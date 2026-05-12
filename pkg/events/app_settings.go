package events

import (
	"errors"
	"fmt"
	"slices"

	"github.com/nbd-wtf/go-nostr"
)

const KindAppSettings = 30078

var validAppSettingsDs = []string{
	"zapstore-unmanaged-apps",
	"zapstore-installed-apps",
}

// AppSettings holds the encrypted application settings.
// For now we only need the "d" tag to be parsed and validated.
type AppSettings struct {
	D string
}

func (a AppSettings) Validate() error {
	if a.D == "" {
		return errors.New("'d' tag is empty")
	}
	if !slices.Contains(validAppSettingsDs, a.D) {
		return errors.New("invalid 'd' tag")
	}
	return nil
}

// ParseAppSettings extracts an AppSettings from a nostr.Event.
// Returns an error if the event kind is wrong or if duplicate singular tags are found.
func ParseAppSettings(e *nostr.Event) (AppSettings, error) {
	if e.Kind != KindAppSettings {
		return AppSettings{}, fmt.Errorf("invalid kind: expected %d, got %d", KindAppSettings, e.Kind)
	}

	settings := AppSettings{}
	for _, tag := range e.Tags {
		if len(tag) < 2 {
			continue
		}

		switch tag[0] {
		case "d":
			if settings.D != "" {
				return AppSettings{}, errors.New("duplicate 'd' tag")
			}
			settings.D = tag[1]
		}
	}
	return settings, nil
}

// ValidateAppSettings parses and validates the app settings event.
func ValidateAppSettings(e *nostr.Event) error {
	settings, err := ParseAppSettings(e)
	if err != nil {
		return err
	}
	return settings.Validate()
}
