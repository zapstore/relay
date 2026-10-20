package relay

import (
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

func TestNotABugReport(t *testing.T) {
	catalog := strings.Repeat("c", 64)
	other := strings.Repeat("d", 64)
	mined := strings.Repeat("0", 4) + strings.Repeat("f", 60)
	short := strings.Repeat("0", 3) + strings.Repeat("f", 61)
	to := func(keys ...string) nostr.Tags {
		tags := nostr.Tags{}
		for _, key := range keys {
			tags = append(tags, nostr.Tag{"p", key})
		}
		return tags
	}

	cases := map[string]struct {
		event  nostr.Event
		reject bool
	}{
		"report":            {nostr.Event{ID: mined, Kind: events.KindGiftWrap, Tags: to(catalog)}, false},
		"too little work":   {nostr.Event{ID: short, Kind: events.KindGiftWrap, Tags: to(catalog)}, true},
		"other recipient":   {nostr.Event{ID: mined, Kind: events.KindGiftWrap, Tags: to(other)}, true},
		"two recipients":    {nostr.Event{ID: mined, Kind: events.KindGiftWrap, Tags: to(catalog, other)}, true},
		"no recipient":      {nostr.Event{ID: mined, Kind: events.KindGiftWrap}, true},
		"oversized":         {nostr.Event{ID: mined, Kind: events.KindGiftWrap, Tags: to(catalog), Content: strings.Repeat("x", maxReportBytes+1)}, true},
		"other kind passes": {nostr.Event{ID: short, Kind: events.KindComment}, false},
	}
	for name, c := range cases {
		err := NotABugReport(catalog)(nil, &c.event)
		if (err != nil) != c.reject {
			t.Errorf("%s: err = %v, want reject %v", name, err, c.reject)
		}
	}
}
