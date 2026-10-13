package relay

import (
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestTooLittleWork(t *testing.T) {
	mined := strings.Repeat("0", 5) + strings.Repeat("f", 59)
	short := strings.Repeat("0", 4) + strings.Repeat("f", 60)

	cases := map[string]struct {
		event  nostr.Event
		reject bool
	}{
		"device state mined":   {nostr.Event{ID: mined, Kind: events.KindAppSettings}, false},
		"device state short":   {nostr.Event{ID: short, Kind: events.KindAppSettings}, true},
		"private stack mined":  {nostr.Event{ID: mined, Kind: events.KindStack, Content: "x"}, false},
		"private stack short":  {nostr.Event{ID: short, Kind: events.KindStack, Content: "x"}, true},
		"public stack":         {nostr.Event{ID: short, Kind: events.KindStack}, false},
		"catalog relay list":   {nostr.Event{ID: short, Kind: events.KindAppRelays}, false},
		"listing needs no pow": {nostr.Event{ID: short, Kind: events.KindApp}, false},
	}
	for name, c := range cases {
		err := TooLittleWork(nil, &c.event)
		if (err != nil) != c.reject {
			t.Errorf("%s: err = %v, want reject %v", name, err, c.reject)
		}
	}
}
