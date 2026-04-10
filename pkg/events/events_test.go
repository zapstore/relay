package events

import (
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

// validHash is a well-formed SHA-256 hex string used in asset test fixtures.
const (
	validHash     = "a000000000000000000000000000000000000000000000000000000000000000"
	validPubkey   = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	validRelayURL = "wss://relay.example.com"
)

func TestParseApp_UnknownFTagsIgnored(t *testing.T) {
	event := &nostr.Event{
		Kind: KindApp,
		Tags: nostr.Tags{
			{"d", "com.example.app"},
			{"name", "Example App"},
			{"f", "android-armeabi"},      // unknown — should be ignored
			{"f", "android-arm64-v8a"},    // known — should be kept
			{"f", "android-unknown-arch"}, // unknown — should be ignored
		},
	}

	app, err := ParseApp(event)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(app.Platforms) != 1 || app.Platforms[0] != "android-arm64-v8a" {
		t.Errorf("expected [android-arm64-v8a], got %v", app.Platforms)
	}
}

func TestValidateApp_OnlyUnknownFTags_Rejected(t *testing.T) {
	event := &nostr.Event{
		Kind: KindApp,
		Tags: nostr.Tags{
			{"d", "com.example.app"},
			{"name", "Example App"},
			{"f", "android-armeabi"},
		},
	}

	err := ValidateApp(event)
	if err == nil {
		t.Fatal("expected validation error for all-unknown f tags, got nil")
	}
	if !strings.Contains(err.Error(), "no recognized platform identifier") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestParseAsset_UnknownFTagsIgnored(t *testing.T) {
	event := &nostr.Event{
		Kind: KindAsset,
		Tags: nostr.Tags{
			{"i", "com.example.app"},
			{"x", validHash},
			{"version", "1.0.0"},
			{"f", "android-armeabi"},     // unknown — should be ignored
			{"f", "android-armeabi-v7a"}, // known — should be kept
			{"version_code", "100"},
			{"apk_certificate_hash", validHash},
		},
	}

	asset, err := ParseAsset(event)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(asset.Platforms) != 1 || asset.Platforms[0] != "android-armeabi-v7a" {
		t.Errorf("expected [android-armeabi-v7a], got %v", asset.Platforms)
	}
}

func TestValidateAsset_OnlyUnknownFTags_Rejected(t *testing.T) {
	event := &nostr.Event{
		Kind: KindAsset,
		Tags: nostr.Tags{
			{"i", "com.example.app"},
			{"x", validHash},
			{"version", "1.0.0"},
			{"f", "android-armeabi"},
			{"version_code", "100"},
			{"apk_certificate_hash", validHash},
		},
	}

	err := ValidateAsset(event)
	if err == nil {
		t.Fatal("expected validation error for all-unknown f tags, got nil")
	}
	if !strings.Contains(err.Error(), "no recognized platform identifier") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestParseStack_UnknownFTagsIgnored(t *testing.T) {
	pk := "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	event := &nostr.Event{
		Kind: KindStack,
		Tags: nostr.Tags{
			{"a", "32267:" + pk + ":com.example.app"},
			{"f", "android-armeabi"},   // unknown — should be ignored
			{"f", "android-arm64-v8a"}, // known — should be kept
		},
	}

	stack, err := ParseStack(event)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(stack.Platforms) != 1 || stack.Platforms[0] != "android-arm64-v8a" {
		t.Errorf("expected [android-arm64-v8a], got %v", stack.Platforms)
	}
}

func TestValidateStack_OnlyUnknownFTags_Rejected(t *testing.T) {
	pk := "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	event := &nostr.Event{
		Kind: KindStack,
		Tags: nostr.Tags{
			{"a", "32267:" + pk + ":com.example.app"},
			{"f", "android-armeabi"},
		},
	}

	err := ValidateStack(event)
	if err == nil {
		t.Fatal("expected validation error for all-unknown f tags, got nil")
	}
	if !strings.Contains(err.Error(), "no recognized platform identifier") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// communityEvent is a helper that builds a minimal valid kind-10222 event.
func communityEvent(extraTags ...nostr.Tag) *nostr.Event {
	tags := nostr.Tags{
		{"r", validRelayURL},
		{"content", "General"},
		{"k", "1111"},
	}
	tags = append(tags, extraTags...)
	return &nostr.Event{Kind: KindCommunityCreation, Tags: tags}
}

// TestParseCommunityCreation_HappyPath checks that a fully-populated event is
// parsed into the correct CommunityCreation fields.
func TestParseCommunityCreation_HappyPath(t *testing.T) {
	event := &nostr.Event{
		Kind: KindCommunityCreation,
		Tags: nostr.Tags{
			{"r", "wss://relay1.example.com"},
			{"r", "wss://relay2.example.com"},
			{"blossom", "https://blossom.example.com"},
			{"mint", "https://mint.example.com", "cashu"},
			{"content", "General"},
			{"k", "1111"},
			{"k", "7"},
			{"a", "30000:" + validPubkey + ":General", validRelayURL},
			{"badge", "30009:" + validPubkey + ":member"},
			{"content", "Apps"},
			{"k", "32267"},
			{"a", "30000:" + validPubkey + ":Apps", validRelayURL},
			{"tos", "abc123"},
			{"location", "Berlin"},
			{"g", "u33d"},
			{"description", "A great community"},
		},
	}

	c, err := ParseCommunityCreation(event)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}

	// Top-level fields
	if len(c.Relays) != 2 {
		t.Errorf("expected 2 relays, got %d", len(c.Relays))
	}
	if len(c.Blossoms) != 1 || c.Blossoms[0] != "https://blossom.example.com" {
		t.Errorf("unexpected blossoms: %v", c.Blossoms)
	}
	if len(c.Mints) != 1 || c.Mints[0] != "https://mint.example.com" {
		t.Errorf("unexpected mints: %v", c.Mints)
	}
	if c.ToS != "abc123" {
		t.Errorf("expected ToS abc123, got %q", c.ToS)
	}
	if c.Location != "Berlin" {
		t.Errorf("expected location Berlin, got %q", c.Location)
	}
	if c.GeoHash != "u33d" {
		t.Errorf("expected geohash u33d, got %q", c.GeoHash)
	}
	if c.Description != "A great community" {
		t.Errorf("expected description, got %q", c.Description)
	}

	// Sections
	if len(c.Sections) != 2 {
		t.Fatalf("expected 2 sections, got %d", len(c.Sections))
	}

	general := c.Sections[0]
	if general.Name != "General" {
		t.Errorf("expected section name General, got %q", general.Name)
	}
	if len(general.Kinds) != 2 {
		t.Errorf("expected 2 kinds in General, got %d", len(general.Kinds))
	}
	if len(general.Lists) != 1 {
		t.Errorf("expected 1 list in General, got %d", len(general.Lists))
	}
	if len(general.Badges) != 1 {
		t.Errorf("expected 1 badge in General, got %d", len(general.Badges))
	}

	apps := c.Sections[1]
	if apps.Name != "Apps" {
		t.Errorf("expected section name Apps, got %q", apps.Name)
	}
	if len(apps.Kinds) != 1 || apps.Kinds[0] != 32267 {
		t.Errorf("expected kinds [32267] in Apps, got %v", apps.Kinds)
	}
}

// TestParseCommunityCreation_WrongKind checks that the wrong event kind is rejected.
func TestParseCommunityCreation_WrongKind(t *testing.T) {
	event := &nostr.Event{Kind: KindApp}
	_, err := ParseCommunityCreation(event)
	if err == nil {
		t.Fatal("expected error for wrong kind, got nil")
	}
}

// TestParseCommunityCreation_KTagsBeforeFirstContent checks that k/a/badge tags
// that appear before the first "content" tag are silently dropped.
func TestParseCommunityCreation_KTagsBeforeFirstContent(t *testing.T) {
	event := &nostr.Event{
		Kind: KindCommunityCreation,
		Tags: nostr.Tags{
			{"r", validRelayURL},
			{"k", "1111"}, // orphan — no content section yet
			{"content", "General"},
			{"k", "7"},
		},
	}

	c, err := ParseCommunityCreation(event)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(c.Sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(c.Sections))
	}
	if len(c.Sections[0].Kinds) != 1 || c.Sections[0].Kinds[0] != 7 {
		t.Errorf("expected only kind 7 in General, got %v", c.Sections[0].Kinds)
	}
}

// TestParseCommunityCreation_InvalidKTagIgnored checks that a non-integer "k" tag
// value is silently skipped.
func TestParseCommunityCreation_InvalidKTagIgnored(t *testing.T) {
	event := &nostr.Event{
		Kind: KindCommunityCreation,
		Tags: nostr.Tags{
			{"r", validRelayURL},
			{"content", "General"},
			{"k", "not-a-number"}, // invalid — should be ignored
			{"k", "1111"},
		},
	}

	c, err := ParseCommunityCreation(event)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(c.Sections[0].Kinds) != 1 || c.Sections[0].Kinds[0] != 1111 {
		t.Errorf("expected only kind 1111, got %v", c.Sections[0].Kinds)
	}
}

// TestValidateCommunityCreation_Valid checks that a well-formed event passes validation.
func TestValidateCommunityCreation_Valid(t *testing.T) {
	if err := ValidateCommunityCreation(communityEvent()); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

// TestValidateCommunityCreation_MissingRelay checks that omitting all relay URLs
// is rejected.
func TestValidateCommunityCreation_MissingRelay(t *testing.T) {
	event := &nostr.Event{
		Kind: KindCommunityCreation,
		Tags: nostr.Tags{
			{"content", "General"},
			{"k", "1111"},
		},
	}
	err := ValidateCommunityCreation(event)
	if err == nil {
		t.Fatal("expected error for missing relay, got nil")
	}
	if !strings.Contains(err.Error(), "'r' tag") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestValidateCommunityCreation_InvalidRelayURL checks that a malformed relay URL
// is rejected.
func TestValidateCommunityCreation_InvalidRelayURL(t *testing.T) {
	event := &nostr.Event{
		Kind: KindCommunityCreation,
		Tags: nostr.Tags{
			{"r", "not-a-url"},
			{"content", "General"},
			{"k", "1111"},
		},
	}
	err := ValidateCommunityCreation(event)
	if err == nil {
		t.Fatal("expected error for invalid relay URL, got nil")
	}
	if !strings.Contains(err.Error(), "invalid relay URL") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestValidateCommunityCreation_MissingContentSection checks that a community with
// no content sections is rejected.
func TestValidateCommunityCreation_MissingContentSection(t *testing.T) {
	event := &nostr.Event{
		Kind: KindCommunityCreation,
		Tags: nostr.Tags{
			{"r", validRelayURL},
		},
	}
	err := ValidateCommunityCreation(event)
	if err == nil {
		t.Fatal("expected error for missing content section, got nil")
	}
	if !strings.Contains(err.Error(), "'content' tag") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestValidateCommunityCreation_SectionMissingKTag checks that a content section
// with no k tags is rejected.
func TestValidateCommunityCreation_SectionMissingKTag(t *testing.T) {
	event := &nostr.Event{
		Kind: KindCommunityCreation,
		Tags: nostr.Tags{
			{"r", validRelayURL},
			{"content", "General"},
			// no k tag
		},
	}
	err := ValidateCommunityCreation(event)
	if err == nil {
		t.Fatal("expected error for section with no k tags, got nil")
	}
	if !strings.Contains(err.Error(), "'k' tags") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestValidateCommunityCreation_InvalidListRef checks that a malformed "a" tag
// reference in a content section is rejected.
func TestValidateCommunityCreation_InvalidListRef(t *testing.T) {
	err := ValidateCommunityCreation(communityEvent(
		nostr.Tag{"a", "99999:not-a-pubkey:General"},
	))
	if err == nil {
		t.Fatal("expected error for invalid list ref, got nil")
	}
	if !strings.Contains(err.Error(), "invalid list ref") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestValidateCommunityCreation_InvalidBadgeRef checks that a malformed "badge" tag
// reference in a content section is rejected.
func TestValidateCommunityCreation_InvalidBadgeRef(t *testing.T) {
	err := ValidateCommunityCreation(communityEvent(
		nostr.Tag{"badge", "not-a-ref"},
	))
	if err == nil {
		t.Fatal("expected error for invalid badge ref, got nil")
	}
	if !strings.Contains(err.Error(), "invalid badge ref") {
		t.Errorf("unexpected error message: %v", err)
	}
}
