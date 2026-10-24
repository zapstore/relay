package main

import (
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestSkipBannedListing(t *testing.T) {
	asset := nostr.Event{Kind: events.KindAsset, Tags: nostr.Tags{{"i", "com.example"}}}
	release := nostr.Event{Kind: events.KindRelease, Tags: nostr.Tags{{"i", "com.example"}}}
	app := nostr.Event{Kind: events.KindApp, Tags: nostr.Tags{{"d", "com.example"}}}
	stack := nostr.Event{Kind: events.KindStack, Tags: nostr.Tags{{"d", "com.example"}}}
	banned := map[string]bool{}
	if skipEvent(asset, banned) {
		t.Fatal("the asset was skipped before the ban")
	}
	banned[eventAppID(asset)] = true
	if !skipEvent(release, banned) || !skipEvent(app, banned) {
		t.Fatal("the rest of the listing was published")
	}
	if skipEvent(stack, banned) {
		t.Fatal("a stack was skipped with the listing")
	}
}
