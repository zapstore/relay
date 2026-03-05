package events

import (
	"fmt"
	"slices"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

const KindAsset = 3063

// Asset represents a parsed Software Asset event (kind 3063).
// Learn more here: https://github.com/franzaps/nips/blob/applications/82.md
type Asset struct {
	// Required fields
	I         string   // App identifier (reverse-domain, e.g. com.example.app)
	Hash      string   // SHA-256 hash of the asset
	Version   string   // Version (e.g., "1.2.3", "0.1.2-rc1")
	Platforms []string // Platform identifiers (at least one required)

	// Optional fields from kind 1063
	URL      string // URL of the asset (may be Blossom URL)
	MimeType string // MIME type of the asset
	Size     string // Size in bytes

	// Software Asset-specific optional fields
	MinPlatformVersion    string   // Minimum platform version
	TargetPlatformVersion string   // Target platform version
	SupportedNIPs         []string // Supported Nostr NIPs
	Filename              string   // Original filename
	Variant               string   // Variant (can be empty)
	Commit                string   // Commit ID for reproducible builds
	MinAllowedVersion     string   // Minimum allowed version
	VersionCode           string   // Version code (Android)
	MinAllowedVersionCode string   // Minimum allowed version code
	APKCertificateHashes  []string // APK certificate hashes (Android)
	Executables           []string // Regex paths to executables (CLI)
}

// Validate checks that all required fields are present and valid.
func (a *Asset) Validate() error {
	if a.I == "" {
		return fmt.Errorf("missing or empty 'i' tag (app identifier)")
	}
	if a.Hash == "" {
		return fmt.Errorf("missing or empty 'x' tag (SHA-256 hash)")
	}
	if a.Version == "" {
		return fmt.Errorf("missing or empty 'version' tag")
	}
	if len(a.Platforms) == 0 {
		return fmt.Errorf("missing 'f' tag (no recognized platform identifier)")
	}

	if err := ValidateHash(a.Hash); err != nil {
		return fmt.Errorf("invalid SHA-256 hash in 'x' tag: %w", err)
	}

	if a.isAndroid() {
		if a.VersionCode == "" {
			return fmt.Errorf("missing or empty 'version_code' tag (required for Android)")
		}
		if len(a.APKCertificateHashes) == 0 {
			return fmt.Errorf("missing 'apk_certificate_hash' tag (required for Android)")
		}
	}
	return nil
}

func (a *Asset) isAndroid() bool {
	for _, p := range a.Platforms {
		if strings.HasPrefix(p, "android-") {
			return true
		}
	}
	return false
}

// ParseAsset extracts a Asset from a nostr.Event.
// Returns an error if the event kind is wrong, content is not empty,
// or if duplicate singular tags are found.
func ParseAsset(event *nostr.Event) (Asset, error) {
	if event.Kind != KindAsset {
		return Asset{}, fmt.Errorf("invalid kind: expected %d, got %d", KindAsset, event.Kind)
	}

	asset := Asset{}
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}

		switch tag[0] {
		case "i":
			if asset.I != "" {
				return Asset{}, fmt.Errorf("duplicate 'i' tag")
			}
			asset.I = tag[1]

		case "x":
			if asset.Hash != "" {
				return Asset{}, fmt.Errorf("duplicate 'x' tag")
			}
			asset.Hash = tag[1]

		case "version":
			if asset.Version != "" {
				return Asset{}, fmt.Errorf("duplicate 'version' tag")
			}
			asset.Version = tag[1]

		case "f":
			if slices.Contains(PlatformIdentifiers, tag[1]) {
				asset.Platforms = append(asset.Platforms, tag[1])
			}

		case "url":
			asset.URL = tag[1]

		case "m":
			if asset.MimeType != "" {
				return Asset{}, fmt.Errorf("duplicate 'm' tag")
			}
			asset.MimeType = tag[1]

		case "size":
			if asset.Size != "" {
				return Asset{}, fmt.Errorf("duplicate 'size' tag")
			}
			asset.Size = tag[1]

		case "min_platform_version":
			if asset.MinPlatformVersion != "" {
				return Asset{}, fmt.Errorf("duplicate 'min_platform_version' tag")
			}
			asset.MinPlatformVersion = tag[1]

		case "target_platform_version":
			if asset.TargetPlatformVersion != "" {
				return Asset{}, fmt.Errorf("duplicate 'target_platform_version' tag")
			}
			asset.TargetPlatformVersion = tag[1]

		case "supported_nip":
			asset.SupportedNIPs = append(asset.SupportedNIPs, tag[1])

		case "filename":
			if asset.Filename != "" {
				return Asset{}, fmt.Errorf("duplicate 'filename' tag")
			}
			asset.Filename = tag[1]

		case "variant":
			if asset.Variant != "" {
				return Asset{}, fmt.Errorf("duplicate 'variant' tag")
			}
			asset.Variant = tag[1]

		case "commit":
			if asset.Commit != "" {
				return Asset{}, fmt.Errorf("duplicate 'commit' tag")
			}
			asset.Commit = tag[1]

		case "min_allowed_version":
			if asset.MinAllowedVersion != "" {
				return Asset{}, fmt.Errorf("duplicate 'min_allowed_version' tag")
			}
			asset.MinAllowedVersion = tag[1]

		case "version_code":
			if asset.VersionCode != "" {
				return Asset{}, fmt.Errorf("duplicate 'version_code' tag")
			}
			asset.VersionCode = tag[1]

		case "min_allowed_version_code":
			if asset.MinAllowedVersionCode != "" {
				return Asset{}, fmt.Errorf("duplicate 'min_allowed_version_code' tag")
			}
			asset.MinAllowedVersionCode = tag[1]

		case "apk_certificate_hash":
			if asset.APKCertificateHashes != nil {
				return Asset{}, fmt.Errorf("duplicate 'apk_certificate_hash' tag")
			}
			asset.APKCertificateHashes = append(asset.APKCertificateHashes, tag[1])

		case "executable":
			asset.Executables = append(asset.Executables, tag[1])
		}
	}

	return asset, nil
}

// ValidateAsset parses and validates a Software Asset event.
func ValidateAsset(event *nostr.Event) error {
	asset, err := ParseAsset(event)
	if err != nil {
		return err
	}
	return asset.Validate()
}
