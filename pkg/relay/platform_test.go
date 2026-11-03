package relay

import (
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestUnsupportedPlatform(t *testing.T) {
	const arm64 = "android-arm64-v8a"
	check := UnsupportedPlatform(DefaultPlatforms)
	ok := &nostr.Event{
		Kind: events.KindApp,
		Tags: nostr.Tags{{"f", arm64}, {"f", "android-armeabi"}},
	}
	if err := check(nil, ok); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []int{events.KindApp, events.KindRelease, events.KindAsset, events.KindStack} {
		for _, platform := range []string{"android-armeabi-v7a", "android-x86_64", "linux-x86_64", "web"} {
			event := &nostr.Event{Kind: kind, Tags: nostr.Tags{{"f", platform}}}
			if err := check(nil, event); err == nil {
				t.Fatalf("kind %d accepted %s", kind, platform)
			}
		}
		mixed := &nostr.Event{Kind: kind, Tags: nostr.Tags{{"f", arm64}, {"f", "android-armeabi-v7a"}, {"f", "android-x86_64"}}}
		if err := check(nil, mixed); err != nil {
			t.Fatalf("kind %d rejected a universal build: %v", kind, err)
		}
		if err := check(nil, &nostr.Event{Kind: kind}); err == nil {
			t.Fatalf("kind %d accepted no platform", kind)
		}
	}
	if err := check(nil, &nostr.Event{Kind: events.KindProfile, Tags: nostr.Tags{{"f", "web"}}}); err != nil {
		t.Fatal(err)
	}
	err := check(nil, &nostr.Event{Kind: events.KindAsset, Tags: nostr.Tags{{"f", "web"}}})
	if err == nil || err.Error() != "platform must include "+arm64 {
		t.Fatalf("error = %v", err)
	}
}

func TestUnsupportedPlatformList(t *testing.T) {
	check := UnsupportedPlatform([]string{"android-arm64-v8a", "linux-x86_64"})
	for _, platform := range []string{"android-arm64-v8a", "linux-x86_64"} {
		event := &nostr.Event{Kind: events.KindAsset, Tags: nostr.Tags{{"f", platform}}}
		if err := check(nil, event); err != nil {
			t.Fatalf("%s rejected: %v", platform, err)
		}
	}
	err := check(nil, &nostr.Event{Kind: events.KindAsset, Tags: nostr.Tags{{"f", "web"}}})
	if err == nil || err.Error() != "platform must include android-arm64-v8a or linux-x86_64" {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigPlatforms(t *testing.T) {
	cfg := NewConfig()
	if len(cfg.Platforms) != 1 || cfg.Platforms[0] != "android-arm64-v8a" {
		t.Fatalf("default platforms = %v", cfg.Platforms)
	}
	cfg.Platforms[0] = "changed"
	if DefaultPlatforms[0] != "android-arm64-v8a" {
		t.Fatal("the config shares the default list")
	}
	for _, bad := range [][]string{nil, {}, {""}, {" web"}} {
		cfg := NewConfig()
		cfg.Hostname = "relay.example.com"
		cfg.Platforms = bad
		if err := cfg.Validate(); err == nil {
			t.Fatalf("platforms %q validated", bad)
		}
	}
}

func TestNotMainChannel(t *testing.T) {
	release := func(tags ...nostr.Tag) *nostr.Event {
		return &nostr.Event{Kind: events.KindRelease, Tags: tags}
	}
	for name, event := range map[string]*nostr.Event{
		"main":         release(nostr.Tag{"c", "main"}),
		"no c tag":     release(),
		"empty c tag":  release(nostr.Tag{"c", ""}),
		"another kind": {Kind: events.KindAsset, Tags: nostr.Tags{{"c", "beta"}}},
		"app with a c": {Kind: events.KindApp, Tags: nostr.Tags{{"c", "nightly"}}},
	} {
		if err := NotMainChannel(nil, event); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, channel := range []string{"beta", "nightly", "Main", "main "} {
		err := NotMainChannel(nil, release(nostr.Tag{"c", channel}))
		if err == nil || !strings.Contains(err.Error(), channel) || !strings.Contains(err.Error(), "main") {
			t.Errorf("channel %q: %v", channel, err)
		}
	}
}
