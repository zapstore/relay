package events

import (
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

// validHash is a well-formed SHA-256 hex string used in asset test fixtures.
const validHash = "a000000000000000000000000000000000000000000000000000000000000000"

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
