package relay

import (
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestOnlyDeletionsSelectHidden(t *testing.T) {
	for kind, want := range map[int]bool{
		nostr.KindDeletion:       true,
		events.KindIdentityProof: true,
		events.KindApp:           false,
		events.KindRelease:       false,
		events.KindAsset:         false,
		events.KindComment:       false,
		events.KindZap:           false,
		events.KindProfile:       false,
		events.KindStack:         false,
	} {
		if got := deletesListings(kind); got != want {
			t.Errorf("kind %d: %v, want %v", kind, got, want)
		}
	}
}
