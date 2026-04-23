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

// --- queryImpressionsSQL ---

func TestQueryImpressionsSQL(t *testing.T) {
	tests := []struct {
		name     string
		filter   ImpressionFilter
		wantSQL  string
		wantArgs []any
	}{
		{
			name:     "no filters no group by",
			filter:   ImpressionFilter{From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM impressions WHERE day >= ? AND day <= ?",
			wantArgs: []any{"2024-01-01", "2024-01-31"},
		},
		{
			name:     "group by only emits SELECT cols and GROUP BY clause",
			filter:   ImpressionFilter{From: "2024-01-01", To: "2024-01-31", GroupBy: []string{"app_id", "day"}},
			wantSQL:  "SELECT app_id, day, SUM(count) AS count FROM impressions WHERE day >= ? AND day <= ? GROUP BY app_id, day ORDER BY day DESC",
			wantArgs: []any{"2024-01-01", "2024-01-31"},
		},
		{
			name:     "app_id filter",
			filter:   ImpressionFilter{AppID: "com.example.app", From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM impressions WHERE app_id = ? AND day >= ? AND day <= ?",
			wantArgs: []any{"com.example.app", "2024-01-01", "2024-01-31"},
		},
		{
			name:     "app_pubkey filter",
			filter:   ImpressionFilter{AppPubkey: "deadbeef", From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM impressions WHERE app_pubkey = ? AND day >= ? AND day <= ?",
			wantArgs: []any{"deadbeef", "2024-01-01", "2024-01-31"},
		},
		{
			name:     "source filter",
			filter:   ImpressionFilter{Source: SourceApp, From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM impressions WHERE day >= ? AND day <= ? AND source = ?",
			wantArgs: []any{"2024-01-01", "2024-01-31", SourceApp},
		},
		{
			name:     "type filter",
			filter:   ImpressionFilter{Type: TypeDetail, From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM impressions WHERE day >= ? AND day <= ? AND type = ?",
			wantArgs: []any{"2024-01-01", "2024-01-31", TypeDetail},
		},
		{
			name: "all filters no group by",
			filter: ImpressionFilter{
				AppID:     "com.example.app",
				AppPubkey: "deadbeef",
				From:      "2024-01-01",
				To:        "2024-01-31",
				Source:    SourceWeb,
				Type:      TypeDetail,
			},
			wantSQL:  "SELECT SUM(count) AS count FROM impressions WHERE app_id = ? AND app_pubkey = ? AND day >= ? AND day <= ? AND source = ? AND type = ?",
			wantArgs: []any{"com.example.app", "deadbeef", "2024-01-01", "2024-01-31", SourceWeb, TypeDetail},
		},
		{
			name: "all filters with group by",
			filter: ImpressionFilter{
				AppID:     "com.example.app",
				AppPubkey: "deadbeef",
				From:      "2024-01-01",
				To:        "2024-01-31",
				Source:    SourceApp,
				Type:      TypeDetail,
				GroupBy:   []string{"day", "source"},
			},
			wantSQL:  "SELECT day, source, SUM(count) AS count FROM impressions WHERE app_id = ? AND app_pubkey = ? AND day >= ? AND day <= ? AND source = ? AND type = ? GROUP BY day, source ORDER BY day DESC",
			wantArgs: []any{"com.example.app", "deadbeef", "2024-01-01", "2024-01-31", SourceApp, TypeDetail},
		},
		{
			name: "pubkey filter with group by app_id and day orders by day desc",
			filter: ImpressionFilter{
				AppPubkey: "deadbeef",
				From:      "2024-01-01",
				To:        "2024-01-31",
				GroupBy:   []string{"app_id", "day"},
			},
			wantSQL:  "SELECT app_id, day, SUM(count) AS count FROM impressions WHERE app_pubkey = ? AND day >= ? AND day <= ? GROUP BY app_id, day ORDER BY day DESC",
			wantArgs: []any{"deadbeef", "2024-01-01", "2024-01-31"},
		},
		{
			name:     "group by does not mutate caller slice",
			filter:   ImpressionFilter{From: "2024-01-01", To: "2024-01-31", GroupBy: []string{"app_id"}},
			wantSQL:  "SELECT app_id, SUM(count) AS count FROM impressions WHERE day >= ? AND day <= ? GROUP BY app_id",
			wantArgs: []any{"2024-01-01", "2024-01-31"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Capture the original GroupBy slice header to detect mutation.
			origGroupBy := make([]string, len(test.filter.GroupBy))
			copy(origGroupBy, test.filter.GroupBy)

			gotSQL, gotArgs := queryImpressionsSQL(test.filter)

			if gotSQL != test.wantSQL {
				t.Errorf("SQL mismatch\n got:  %q\n want: %q", gotSQL, test.wantSQL)
			}
			if !reflect.DeepEqual(gotArgs, test.wantArgs) {
				t.Errorf("args mismatch\n got:  %v\n want: %v", gotArgs, test.wantArgs)
			}
			if !slices.Equal(test.filter.GroupBy, origGroupBy) {
				t.Errorf("GroupBy slice was mutated: got %v, want %v", test.filter.GroupBy, origGroupBy)
			}
		})
	}
}

// --- IsDetailFilter ---

func TestIsDetailFilter(t *testing.T) {
	tests := []struct {
		name   string
		filter nostr.Filter
		want   bool
	}{
		{
			name:   "detail filter (kind + author + d)",
			filter: nostr.Filter{Kinds: []int{eventPkg.KindApp}, Authors: []string{"pubkey"}, Tags: nostr.TagMap{"d": {"com.example.app"}}},
			want:   true,
		},
		{
			name:   "missing author",
			filter: nostr.Filter{Kinds: []int{eventPkg.KindApp}, Tags: nostr.TagMap{"d": {"com.example.app"}}},
			want:   false,
		},
		{
			name:   "missing d tag",
			filter: nostr.Filter{Kinds: []int{eventPkg.KindApp}, Authors: []string{"pubkey"}},
			want:   false,
		},
		{
			name:   "search filter (no d tag)",
			filter: nostr.Filter{Kinds: []int{eventPkg.KindApp}, Authors: []string{"pubkey"}, Search: "signal"},
			want:   false,
		},
		{
			name:   "wrong kind",
			filter: nostr.Filter{Kinds: []int{eventPkg.KindStack}, Authors: []string{"pubkey"}, Tags: nostr.TagMap{"d": {"com.example.app"}}},
			want:   false,
		},
		{
			name:   "empty filter",
			filter: nostr.Filter{},
			want:   false,
		},
		{
			name:   "unrelated kind with author and d tag",
			filter: nostr.Filter{Kinds: []int{0, 1}, Authors: []string{"pubkey"}, Tags: nostr.TagMap{"d": {"something"}}},
			want:   false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsDetailFilter(test.filter); got != test.want {
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
			name:    "detail impression from app source",
			id:      "app-detail-com.example.app1",
			filters: nostr.Filters{{Kinds: []int{eventPkg.KindApp}, Authors: []string{"pubkey"}, Tags: nostr.TagMap{"d": {"com.example.app1"}}}},
			events: []nostr.Event{
				appEvent("com.example.app1", "pubkey"),
				appEvent("com.example.app2", "pubkey"),
			},
			want: []Impression{
				{AppID: "com.example.app1", AppPubkey: "pubkey", Day: day, Source: SourceApp, Type: TypeDetail},
			},
		},
		{
			name:    "detail impression from web source",
			id:      "web-app-detail-123456",
			filters: nostr.Filters{{Kinds: []int{eventPkg.KindApp}, Authors: []string{"pubkey"}, Tags: nostr.TagMap{"d": {"com.example.app1"}}}},
			events: []nostr.Event{
				appEvent("com.example.app1", "pubkey"),
			},
			want: []Impression{
				{AppID: "com.example.app1", AppPubkey: "pubkey", Day: day, Source: SourceWeb, Type: TypeDetail},
			},
		},
		{
			name:    "unknown source is ignored",
			id:      "other-req-1",
			filters: nostr.Filters{{Kinds: []int{eventPkg.KindApp}, Authors: []string{"pubkey"}, Tags: nostr.TagMap{"d": {"com.example.app1"}}}},
			events: []nostr.Event{
				appEvent("com.example.app1", "pubkey"),
			},
			want: nil,
		},
		{
			name:    "broad app- prefix is not enough",
			id:      "app-search-results",
			filters: nostr.Filters{{Kinds: []int{eventPkg.KindApp}, Authors: []string{"pubkey"}, Tags: nostr.TagMap{"d": {"com.example.app1"}}}},
			events: []nostr.Event{
				appEvent("com.example.app1", "pubkey"),
			},
			want: nil,
		},
		{
			name:    "broad web- prefix is not enough",
			id:      "web-q-999",
			filters: nostr.Filters{{Kinds: []int{eventPkg.KindApp}, Authors: []string{"pubkey"}, Tags: nostr.TagMap{"d": {"com.example.app1"}}}},
			events: []nostr.Event{
				appEvent("com.example.app1", "pubkey"),
			},
			want: nil,
		},
		{
			name:    "missing author is ignored",
			id:      "app-detail-com.example.app1",
			filters: nostr.Filters{{Kinds: []int{eventPkg.KindApp}, Tags: nostr.TagMap{"d": {"com.example.app1"}}}},
			events: []nostr.Event{
				appEvent("com.example.app1", "pubkey"),
			},
			want: []Impression{},
		},
		{
			name:    "feed filter is ignored",
			id:      "app-detail-com.example.app1",
			filters: nostr.Filters{{Kinds: []int{eventPkg.KindApp}, Authors: []string{"pubkey"}}},
			events: []nostr.Event{
				appEvent("com.example.app1", "pubkey"),
			},
			want: []Impression{},
		},
		{
			name:    "search filter is ignored",
			id:      "app-detail-com.example.app1",
			filters: nostr.Filters{{Kinds: []int{eventPkg.KindApp}, Authors: []string{"pubkey"}, Search: "signal"}},
			events: []nostr.Event{
				appEvent("com.example.app1", "pubkey"),
			},
			want: []Impression{},
		},
		{
			name:    "stack filter is ignored",
			id:      "web-app-detail-789",
			filters: nostr.Filters{{Kinds: []int{eventPkg.KindStack}}},
			events: []nostr.Event{
				stackEvent("32267:pubkey:com.example.app1"),
			},
			want: []Impression{},
		},
		{
			name:    "empty d tag event is skipped",
			id:      "app-detail-com.example.app1",
			filters: nostr.Filters{{Kinds: []int{eventPkg.KindApp}, Authors: []string{"pubkey"}, Tags: nostr.TagMap{"d": {"com.example.app1"}}}},
			events: []nostr.Event{
				appEvent("", "pubkey"),
			},
			want: []Impression{},
		},
		{
			name: "mixed filters: only detail filter counted",
			id:   "web-app-detail-456",
			filters: nostr.Filters{
				{Kinds: []int{eventPkg.KindStack}},
				{Kinds: []int{eventPkg.KindApp}, Authors: []string{"PUBKEY"}, Tags: nostr.TagMap{"d": {"com.example.app3"}}},
			},
			events: []nostr.Event{
				stackEvent("32267:pubkey:com.example.app1", "32267:pubkey:com.example.app2"),
				appEvent("com.example.app3", "PUBKEY"),
			},
			want: []Impression{
				{AppID: "com.example.app3", AppPubkey: "PUBKEY", Day: day, Source: SourceWeb, Type: TypeDetail},
			},
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
				{Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: TypeDetail}, 1},
			},
			want: []ImpressionCount{
				{Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: TypeDetail}, 1},
			},
		},
		{
			name: "count is persisted correctly",
			batch: []ImpressionCount{
				{Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: TypeDetail}, 42},
			},
			want: []ImpressionCount{
				{Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: TypeDetail}, 42},
			},
		},
		{
			name: "multiple distinct impressions",
			batch: []ImpressionCount{
				{Impression{AppID: "com.example.app1", Day: "2024-01-01", Source: SourceApp, Type: TypeDetail}, 3},
				{Impression{AppID: "com.example.app2", Day: "2024-01-01", Source: SourceWeb, Type: TypeDetail}, 7},
				{Impression{AppID: "com.example.app1", Day: "2024-01-02", Source: SourceApp, Type: TypeDetail}, 1},
			},
			want: []ImpressionCount{
				{Impression{AppID: "com.example.app1", Day: "2024-01-01", Source: SourceApp, Type: TypeDetail}, 3},
				{Impression{AppID: "com.example.app2", Day: "2024-01-01", Source: SourceWeb, Type: TypeDetail}, 7},
				{Impression{AppID: "com.example.app1", Day: "2024-01-02", Source: SourceApp, Type: TypeDetail}, 1},
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

	imp := Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: TypeDetail}

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

func stackEvent(aTags ...string) nostr.Event {
	tags := nostr.Tags{}
	for _, aTag := range aTags {
		tags = append(tags, nostr.Tag{"a", aTag})
	}
	return nostr.Event{
		Kind: eventPkg.KindStack,
		Tags: tags,
	}
}
