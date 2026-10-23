package relay

import (
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestOnlyListingChangesReselect(t *testing.T) {
	for kind, want := range map[int]bool{
		nostr.KindDeletion:       true,
		events.KindApp:           true,
		events.KindRelease:       true,
		events.KindAsset:         true,
		events.KindIdentityProof: true,
		events.KindComment:       false,
		events.KindZap:           false,
		events.KindProfile:       false,
		events.KindStack:         false,
	} {
		if got := changesListings(kind); got != want {
			t.Errorf("kind %d: %v, want %v", kind, got, want)
		}
	}
}
