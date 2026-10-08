package relay

import (
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestNotARM64(t *testing.T) {
	ok := &nostr.Event{
		Kind: events.KindApp,
		Tags: nostr.Tags{{"f", platformARM64}, {"f", "android-armeabi"}},
	}
	if err := NotARM64(nil, ok); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []int{events.KindApp, events.KindRelease, events.KindAsset, events.KindStack} {
		for _, platform := range []string{"android-armeabi-v7a", "android-x86_64", "linux-x86_64", "web"} {
			event := &nostr.Event{Kind: kind, Tags: nostr.Tags{{"f", platform}}}
			if err := NotARM64(nil, event); err == nil {
				t.Fatalf("kind %d accepted %s", kind, platform)
			}
		}
		mixed := &nostr.Event{Kind: kind, Tags: nostr.Tags{{"f", platformARM64}, {"f", "android-armeabi-v7a"}, {"f", "android-x86_64"}}}
		if err := NotARM64(nil, mixed); err != nil {
			t.Fatalf("kind %d rejected a universal build: %v", kind, err)
		}
		if err := NotARM64(nil, &nostr.Event{Kind: kind}); err == nil {
			t.Fatalf("kind %d accepted no platform", kind)
		}
	}
	if err := NotARM64(nil, &nostr.Event{Kind: events.KindProfile, Tags: nostr.Tags{{"f", "web"}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(NotARM64(nil, &nostr.Event{Kind: events.KindAsset, Tags: nostr.Tags{{"f", "web"}}}).Error(), platformARM64) {
		t.Fatal("error does not name the platform")
	}
}
