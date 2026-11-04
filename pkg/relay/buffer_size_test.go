package relay

import (
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestTooLargeForBuffer(t *testing.T) {
	big := strings.Repeat("x", maxBufferedEventBytes)

	cases := map[string]struct {
		event  nostr.Event
		reject bool
	}{
		"app ok":            {nostr.Event{Kind: events.KindApp}, false},
		"app oversized":     {nostr.Event{Kind: events.KindApp, Content: big}, true},
		"release oversized": {nostr.Event{Kind: events.KindRelease, Content: big}, true},
		"asset oversized":   {nostr.Event{Kind: events.KindAsset, Content: big}, true},
		"proof oversized":   {nostr.Event{Kind: events.KindIdentityProof, Content: big}, true},
		"other kind passes": {nostr.Event{Kind: events.KindComment, Content: big}, false},
	}
	for name, c := range cases {
		err := TooLargeForBuffer(nil, &c.event)
		if (err != nil) != c.reject {
			t.Errorf("%s: err = %v, want reject %v", name, err, c.reject)
		}
	}
}
