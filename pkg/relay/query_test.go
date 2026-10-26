package relay

import (
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

func TestReqSummary(t *testing.T) {
	since := nostr.Timestamp(1)
	filters := nostr.Filters{
		{
			Kinds:  []int{3063},
			Limit:  1,
			Tags:   nostr.TagMap{"i": []string{"com.example.one"}, "f": []string{"android-arm64-v8a"}},
			Search: "hidden",
		},
		{
			Kinds:   []int{3063},
			Limit:   1,
			Authors: []string{"aa"},
			IDs:     []string{"bb"},
			Since:   &since,
			Until:   &since,
			Tags:    nostr.TagMap{"i": []string{"com.example.two"}},
		},
	}
	got := reqSummary(filters)
	want := "n=2 kinds=3063 tags=f,i limit=1 search=1 authors=1 ids=1 since=1 until=1"
	if got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}

	mixed := nostr.Filters{
		{Kinds: []int{32267, 3063}, Limit: 1},
		{LimitZero: true},
	}
	got = reqSummary(mixed)
	want = "n=2 kinds=3063,32267 limit=mixed"
	if got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}

	if got := reqSummary(nil); got != "n=0" {
		t.Fatalf("empty = %q", got)
	}
	if got := reqSummary(nostr.Filters{{LimitZero: true}}); got != "n=1 limit=0" {
		t.Fatalf("limit zero = %q", got)
	}
}
