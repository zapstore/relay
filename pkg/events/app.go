// Package events provides functions for validating events structures.
// It also exposes normalized structures and parsing functions for Zapstore events.
package events

import (
	"fmt"
	"slices"

	"github.com/nbd-wtf/go-nostr"
)

const KindApp = 32267

var PlatformIdentifiers = []string{
	"android-arm64-v8a",
	"android-armeabi-v7a",
	"android-x86",
	"android-x86_64",
	"darwin-arm64",
	"darwin-x86_64",
	"linux-aarch64",
	"linux-x86_64",
	"linux-armv7l",
	"linux-riscv64",
	"windows-aarch64",
	"windows-x86_64",
	"ios-arm64",
	"web",
}

// App represents a parsed Software Application event (kind 32267).
// Learn more here: https://github.com/franzaps/nips/blob/applications/82.md
type App struct {
	// Required fields
	D         string   // App identifier (reverse-domain recommended, e.g. com.example.app)
	Name      string   // Human-readable project name
	Platforms []string // Platform identifiers (at least one required)

	// Optional fields
	Content    string   // Full description, markdown allowed
	Summary    string   // Short description, no markdown
	Icon       string   // Icon URL
	Images     []string // Image URLs
	Tags       []string // Tags describing the application
	URL        string   // Website URL
	Repository string   // Source code repository URL
	License    string   // SPDX license ID
}

func (app App) Validate() error {
	if app.D == "" {
		return fmt.Errorf("missing or empty 'd' tag (app identifier)")
	}
	if app.Name == "" {
		return fmt.Errorf("missing or empty 'name' tag")
	}
	if len(app.Platforms) == 0 {
		return fmt.Errorf("missing 'f' tag (no recognized platform identifier)")
	}
	return nil
}

// ParseApp extracts a App from a nostr.Event.
// Returns an error if the event kind is wrong or if duplicate singular tags are found.
func ParseApp(event *nostr.Event) (App, error) {
	if event.Kind != KindApp {
		return App{}, fmt.Errorf("invalid kind: expected %d, got %d", KindApp, event.Kind)
	}

	app := App{Content: event.Content}
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}

		switch tag[0] {
		case "d":
			if app.D != "" {
				return App{}, fmt.Errorf("duplicate 'd' tag")
			}
			app.D = tag[1]

		case "name":
			if app.Name != "" {
				return App{}, fmt.Errorf("duplicate 'name' tag")
			}
			app.Name = tag[1]

		case "f":
			if slices.Contains(PlatformIdentifiers, tag[1]) {
				app.Platforms = append(app.Platforms, tag[1])
			}

		case "summary":
			if app.Summary != "" {
				return App{}, fmt.Errorf("duplicate 'summary' tag")
			}
			app.Summary = tag[1]

		case "icon":
			if app.Icon != "" {
				return App{}, fmt.Errorf("duplicate 'icon' tag")
			}
			app.Icon = tag[1]

		case "image":
			app.Images = append(app.Images, tag[1])

		case "t":
			app.Tags = append(app.Tags, tag[1])

		case "url":
			app.URL = tag[1]

		case "repository":
			if app.Repository != "" {
				return App{}, fmt.Errorf("duplicate 'repository' tag")
			}
			app.Repository = tag[1]

		case "license":
			if app.License != "" {
				return App{}, fmt.Errorf("duplicate 'license' tag")
			}
			app.License = tag[1]
		}
	}
	return app, nil
}

// ValidateApp parses and validates a Software Application event.
func ValidateApp(event *nostr.Event) error {
	app, err := ParseApp(event)
	if err != nil {
		return err
	}
	return app.Validate()
}
