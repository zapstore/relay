package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	eventPkg "github.com/zapstore/relay/pkg/events"
)

// --- ImpressionType ---

func TestImpressionType(t *testing.T) {
	tests := []struct {
		name   string
		filter nostr.Filter
		want   Type
	}{
		{
			name:   "stack filter",
			filter: nostr.Filter{Kinds: []int{eventPkg.KindAppSet}},
			want:   TypeStack,
		},
		{
			name:   "feed filter",
			filter: nostr.Filter{Kinds: []int{eventPkg.KindApp}},
			want:   TypeFeed,
		},
		{
			name:   "search filter",
			filter: nostr.Filter{Kinds: []int{eventPkg.KindApp}, Search: "signal"},
			want:   TypeSearch,
		},
		{
			name:   "detail filter",
			filter: nostr.Filter{Kinds: []int{eventPkg.KindApp}, Tags: nostr.TagMap{"d": {"com.example.app"}}},
			want:   TypeDetail,
		},
		{
			name:   "empty filter",
			filter: nostr.Filter{},
			want:   TypeUndetermined,
		},
		{
			name:   "undetermined filter",
			filter: nostr.Filter{Kinds: []int{0, 1}, Tags: nostr.TagMap{"p": {"5c50da132947fa3bf4759eb978d784db12baad1c3e5b6a575410aeb654639b4b"}}},
			want:   TypeUndetermined,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ImpressionType(test.filter); got != test.want {
				t.Errorf("got %v, want %v", got, test.want)
			}
		})
	}
}

// --- NewImpressions ---

func TestNewImpressions(t *testing.T) {
	day := Today()

	tests := []struct {
		name    string
		id      string
		filters nostr.Filters
		events  []nostr.Event
		want    []Impression
	}{
		{
			name:    "feed impressions",
			id:      "app-req-1",
			filters: nostr.Filters{{Kinds: []int{eventPkg.KindApp}}},
			events: []nostr.Event{
				appEvent("com.example.app1", "pk1"),
				appEvent("com.example.app2", "pk2"),
				appEvent("com.example.app3", "pk3"),
			},
			want: []Impression{
				{AppID: "com.example.app1", AppPubkey: "pk1", Day: day, Source: SourceApp, Type: TypeFeed},
				{AppID: "com.example.app2", AppPubkey: "pk2", Day: day, Source: SourceApp, Type: TypeFeed},
				{AppID: "com.example.app3", AppPubkey: "pk3", Day: day, Source: SourceApp, Type: TypeFeed},
			},
		},
		{
			name:    "detail impressions for matching d tag",
			id:      "web-req-1",
			filters: nostr.Filters{{Kinds: []int{eventPkg.KindApp}, Tags: nostr.TagMap{"d": {"com.example.app1"}}}},
			events: []nostr.Event{
				appEvent("com.example.app1", "pubkey"),
				appEvent("com.example.app2", "pubkey"), // skipped since it doesn't match the filter
			},
			want: []Impression{
				{AppID: "com.example.app1", AppPubkey: "pubkey", Day: day, Source: SourceWeb, Type: TypeDetail},
			},
		},
		{
			name:    "search impressions skip empty d",
			id:      "app-req-2",
			filters: nostr.Filters{{Kinds: []int{eventPkg.KindApp}, Search: "signal"}},
			events: []nostr.Event{
				appEvent("com.example.app1", "pubkey"),
				appEvent("", "pubkey"),
			},
			want: []Impression{
				{AppID: "com.example.app1", AppPubkey: "pubkey", Day: day, Source: SourceApp, Type: TypeSearch},
			},
		},
		{
			name:    "stack impressions from app set",
			id:      "web-req-2",
			filters: nostr.Filters{{Kinds: []int{eventPkg.KindAppSet}}},
			events: []nostr.Event{
				appSetEvent("32267:pubkey:com.example.app1", "32267:pubkey:com.example.app2"),
				appEvent("com.example.app1", "pubkey"), // not considered because doesn't match the filter
			},
			want: []Impression{
				{AppID: "com.example.app1", AppPubkey: "pubkey", Day: day, Source: SourceWeb, Type: TypeStack},
				{AppID: "com.example.app2", AppPubkey: "pubkey", Day: day, Source: SourceWeb, Type: TypeStack},
			},
		},
		{
			name: "stack impressions from app set and app",
			id:   "web-req-2",
			filters: nostr.Filters{
				{Kinds: []int{eventPkg.KindAppSet}},
				{Kinds: []int{eventPkg.KindApp}, Tags: nostr.TagMap{"d": {"com.example.app3"}}},
			},
			events: []nostr.Event{
				appSetEvent("32267:pubkey:com.example.app1", "32267:pubkey:com.example.app2"),
				appEvent("com.example.app3", "PUBKEY"),
			},
			want: []Impression{
				{AppID: "com.example.app1", AppPubkey: "pubkey", Day: day, Source: SourceWeb, Type: TypeStack},
				{AppID: "com.example.app2", AppPubkey: "pubkey", Day: day, Source: SourceWeb, Type: TypeStack},
				{AppID: "com.example.app3", AppPubkey: "PUBKEY", Day: day, Source: SourceWeb, Type: TypeDetail},
			},
		},
		{
			name:    "undetermined filter skipped",
			id:      "app-req-3",
			filters: nostr.Filters{{}},
			events: []nostr.Event{
				appEvent("com.example.app1", "pubkey"),
			},
			want: []Impression{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := NewImpressions("", test.id, test.filters, test.events)
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("got %v, want %v", got, test.want)
			}
		})
	}
}

// --- SaveImpressions ---

func TestSaveImpressions(t *testing.T) {
	tests := []struct {
		name  string
		batch []ImpressionCount
		want  []ImpressionCount
	}{
		{
			name:  "empty batch is a no-op",
			batch: nil,
			want:  nil,
		},
		{
			name: "single impression",
			batch: []ImpressionCount{
				{Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: TypeFeed}, 1},
			},
			want: []ImpressionCount{
				{Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: TypeFeed}, 1},
			},
		},
		{
			name: "count is persisted correctly",
			batch: []ImpressionCount{
				{Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: TypeFeed}, 42},
			},
			want: []ImpressionCount{
				{Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: TypeFeed}, 42},
			},
		},
		{
			name: "multiple distinct impressions",
			batch: []ImpressionCount{
				{Impression{AppID: "com.example.app1", Day: "2024-01-01", Source: SourceApp, Type: TypeFeed}, 3},
				{Impression{AppID: "com.example.app2", Day: "2024-01-01", Source: SourceWeb, Type: TypeDetail}, 7},
				{Impression{AppID: "com.example.app1", Day: "2024-01-02", Source: SourceApp, Type: TypeSearch}, 1},
			},
			want: []ImpressionCount{
				{Impression{AppID: "com.example.app1", Day: "2024-01-01", Source: SourceApp, Type: TypeFeed}, 3},
				{Impression{AppID: "com.example.app2", Day: "2024-01-01", Source: SourceWeb, Type: TypeDetail}, 7},
				{Impression{AppID: "com.example.app1", Day: "2024-01-02", Source: SourceApp, Type: TypeSearch}, 1},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, err := New(":memory:")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer s.Close()

			if err := s.SaveImpressions(context.Background(), test.batch); err != nil {
				t.Fatalf("SaveImpressions: %v", err)
			}

			got, err := queryImpressions(s.db)
			if err != nil {
				t.Fatalf("queryImpressions: %v", err)
			}

			sortImpressionCounts(got)
			sortImpressionCounts(test.want)

			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("mismatch\n got: %v\nwant: %v", got, test.want)
			}
		})
	}
}

func TestSaveImpressions_AccumulatesAcrossCalls(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	imp := Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: TypeFeed}

	if err := s.SaveImpressions(context.Background(), []ImpressionCount{{imp, 3}}); err != nil {
		t.Fatalf("first SaveImpressions: %v", err)
	}
	if err := s.SaveImpressions(context.Background(), []ImpressionCount{{imp, 5}}); err != nil {
		t.Fatalf("second SaveImpressions: %v", err)
	}

	got, err := queryImpressions(s.db)
	if err != nil {
		t.Fatalf("queryImpressions: %v", err)
	}

	want := []ImpressionCount{{imp, 8}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mismatch\n got: %v\nwant: %v", got, want)
	}
}

// --- Helpers ---

func queryImpressions(db *sql.DB) ([]ImpressionCount, error) {
	rows, err := db.Query(`SELECT app_id, app_pubkey, day, source, type, country_code, count FROM impressions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []ImpressionCount
	for rows.Next() {
		var (
			appID, appPubkey, day, source, typ, countryCode string
			count                                           int
		)
		if err := rows.Scan(&appID, &appPubkey, &day, &source, &typ, &countryCode, &count); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		results = append(results, ImpressionCount{
			Impression: Impression{
				AppID:       appID,
				AppPubkey:   appPubkey,
				Day:         normalizeDay(day),
				Source:      Source(source),
				Type:        Type(typ),
				CountryCode: countryCode,
			},
			Count: count,
		})
	}
	return results, rows.Err()
}

func sortImpressionCounts(rows []ImpressionCount) {
	slices.SortFunc(rows, func(a, b ImpressionCount) int {
		if c := cmp.Compare(a.AppID, b.AppID); c != 0 {
			return c
		}
		if c := cmp.Compare(a.AppPubkey, b.AppPubkey); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Day, b.Day); c != 0 {
			return c
		}
		if c := cmp.Compare(string(a.Source), string(b.Source)); c != 0 {
			return c
		}
		return cmp.Compare(string(a.Type), string(b.Type))
	})
}

func normalizeDay(day string) string {
	if len(day) >= 10 {
		return day[:10]
	}
	return strings.TrimSpace(day)
}

func appEvent(appID string, pubkey string) nostr.Event {
	tags := nostr.Tags{}
	if appID != "" {
		tags = append(tags, nostr.Tag{"d", appID})
	}
	return nostr.Event{
		PubKey: pubkey,
		Kind:   eventPkg.KindApp,
		Tags:   tags,
	}
}

func appSetEvent(aTags ...string) nostr.Event {
	tags := nostr.Tags{}
	for _, aTag := range aTags {
		tags = append(tags, nostr.Tag{"a", aTag})
	}
	return nostr.Event{
		Kind: eventPkg.KindAppSet,
		Tags: tags,
	}
}
