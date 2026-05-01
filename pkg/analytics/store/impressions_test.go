package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	eventPkg "github.com/zapstore/relay/pkg/events"
)

var (
	ctx     = context.Background()
	pubkey1 = "5c50da132947fa3bf4759eb978d784db12baad1c3e5b6a575410aeb654639b4b"
	pubkey2 = "805b34f708837dfb3e7f05815ac5760564628b58d5a0ce839ccbb6ef3620fac3"
)

// --- QueryImpressions ---

func TestQueryImpressions(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	seed := []ImpressionCount{
		{Impression{AppID: "com.example.app1", AppPubkey: pubkey1, Day: "2024-01-01", Source: SourceApp, Type: ImpressionDetail, CountryCode: "US"}, 10},
		{Impression{AppID: "com.example.app1", AppPubkey: pubkey1, Day: "2024-01-02", Source: SourceApp, Type: ImpressionDetail, CountryCode: "US"}, 5},
		{Impression{AppID: "com.example.app1", AppPubkey: pubkey1, Day: "2024-01-03", Source: SourceWeb, Type: ImpressionDetail, CountryCode: "DE"}, 3},
		{Impression{AppID: "com.example.app2", AppPubkey: pubkey2, Day: "2024-01-01", Source: SourceApp, Type: ImpressionDetail, CountryCode: "US"}, 7},
		{Impression{AppID: "com.example.app2", AppPubkey: pubkey2, Day: "2024-01-02", Source: SourceWeb, Type: ImpressionDetail, CountryCode: "FR"}, 4},
	}
	if err := s.SaveImpressions(ctx, seed); err != nil {
		t.Fatalf("SaveImpressions: %v", err)
	}

	t.Run("total count no group by", func(t *testing.T) {
		got, err := s.QueryImpressions(ctx, ImpressionFilter{
			From: "2024-01-01",
			To:   "2024-01-03",
		})
		if err != nil {
			t.Fatalf("QueryImpressions: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 row, got %d", len(got))
		}
		if got[0].Count != 29 {
			t.Errorf("got count %d, want 29", got[0].Count)
		}
	})

	t.Run("filter by app_id", func(t *testing.T) {
		got, err := s.QueryImpressions(ctx, ImpressionFilter{
			AppID: "com.example.app1",
			From:  "2024-01-01",
			To:    "2024-01-03",
		})
		if err != nil {
			t.Fatalf("QueryImpressions: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 row, got %d", len(got))
		}
		if got[0].Count != 18 {
			t.Errorf("got count %d, want 18", got[0].Count)
		}
	})

	t.Run("filter by app_pubkey", func(t *testing.T) {
		got, err := s.QueryImpressions(ctx, ImpressionFilter{
			AppPubkey: pubkey2,
			From:      "2024-01-01",
			To:        "2024-01-03",
		})
		if err != nil {
			t.Fatalf("QueryImpressions: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 row, got %d", len(got))
		}
		if got[0].Count != 11 {
			t.Errorf("got count %d, want 11", got[0].Count)
		}
	})

	t.Run("filter by source", func(t *testing.T) {
		got, err := s.QueryImpressions(ctx, ImpressionFilter{
			Source: SourceWeb,
			From:   "2024-01-01",
			To:     "2024-01-03",
		})
		if err != nil {
			t.Fatalf("QueryImpressions: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 row, got %d", len(got))
		}
		if got[0].Count != 7 {
			t.Errorf("got count %d, want 7", got[0].Count)
		}
	})

	t.Run("group by day ordered desc", func(t *testing.T) {
		got, err := s.QueryImpressions(ctx, ImpressionFilter{
			From:    "2024-01-01",
			To:      "2024-01-03",
			GroupBy: []string{"day"},
		})
		if err != nil {
			t.Fatalf("QueryImpressions: %v", err)
		}

		want := []ImpressionCount{
			{Impression{Day: "2024-01-03"}, 3},
			{Impression{Day: "2024-01-02"}, 9},
			{Impression{Day: "2024-01-01"}, 17},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("group by app_id and day ordered desc", func(t *testing.T) {
		got, err := s.QueryImpressions(ctx, ImpressionFilter{
			AppPubkey: pubkey1,
			From:      "2024-01-01",
			To:        "2024-01-03",
			GroupBy:   []string{"app_id", "day"},
		})
		if err != nil {
			t.Fatalf("QueryImpressions: %v", err)
		}

		want := []ImpressionCount{
			{Impression{AppID: "com.example.app1", Day: "2024-01-03"}, 3},
			{Impression{AppID: "com.example.app1", Day: "2024-01-02"}, 5},
			{Impression{AppID: "com.example.app1", Day: "2024-01-01"}, 10},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("validation error invalid pubkey", func(t *testing.T) {
		_, err := s.QueryImpressions(ctx, ImpressionFilter{
			AppPubkey: "notahexpubkey",
			From:      "2024-01-01",
			To:        "2024-01-03",
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

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
			wantSQL:  "SELECT SUM(count) AS count FROM app_impressions WHERE day >= ? AND day <= ?",
			wantArgs: []any{"2024-01-01", "2024-01-31"},
		},
		{
			name:     "group by only emits SELECT cols and GROUP BY clause",
			filter:   ImpressionFilter{From: "2024-01-01", To: "2024-01-31", GroupBy: []string{"app_id", "day"}},
			wantSQL:  "SELECT app_id, day, SUM(count) AS count FROM app_impressions WHERE day >= ? AND day <= ? GROUP BY app_id, day ORDER BY day DESC",
			wantArgs: []any{"2024-01-01", "2024-01-31"},
		},
		{
			name:     "app_id filter",
			filter:   ImpressionFilter{AppID: "com.example.app", From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM app_impressions WHERE app_id = ? AND day >= ? AND day <= ?",
			wantArgs: []any{"com.example.app", "2024-01-01", "2024-01-31"},
		},
		{
			name:     "app_pubkey filter",
			filter:   ImpressionFilter{AppPubkey: "deadbeef", From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM app_impressions WHERE app_pubkey = ? AND day >= ? AND day <= ?",
			wantArgs: []any{"deadbeef", "2024-01-01", "2024-01-31"},
		},
		{
			name:     "source filter",
			filter:   ImpressionFilter{Source: SourceApp, From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM app_impressions WHERE day >= ? AND day <= ? AND source = ?",
			wantArgs: []any{"2024-01-01", "2024-01-31", SourceApp},
		},
		{
			name:     "type filter",
			filter:   ImpressionFilter{Type: ImpressionDetail, From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM app_impressions WHERE day >= ? AND day <= ? AND type = ?",
			wantArgs: []any{"2024-01-01", "2024-01-31", ImpressionDetail},
		},
		{
			name: "all filters no group by",
			filter: ImpressionFilter{
				AppID:     "com.example.app",
				AppPubkey: "deadbeef",
				From:      "2024-01-01",
				To:        "2024-01-31",
				Source:    SourceWeb,
				Type:      ImpressionDetail,
			},
			wantSQL:  "SELECT SUM(count) AS count FROM app_impressions WHERE app_id = ? AND app_pubkey = ? AND day >= ? AND day <= ? AND source = ? AND type = ?",
			wantArgs: []any{"com.example.app", "deadbeef", "2024-01-01", "2024-01-31", SourceWeb, ImpressionDetail},
		},
		{
			name: "all filters with group by",
			filter: ImpressionFilter{
				AppID:     "com.example.app",
				AppPubkey: "deadbeef",
				From:      "2024-01-01",
				To:        "2024-01-31",
				Source:    SourceApp,
				Type:      ImpressionDetail,
				GroupBy:   []string{"day", "source"},
			},
			wantSQL:  "SELECT day, source, SUM(count) AS count FROM app_impressions WHERE app_id = ? AND app_pubkey = ? AND day >= ? AND day <= ? AND source = ? AND type = ? GROUP BY day, source ORDER BY day DESC",
			wantArgs: []any{"com.example.app", "deadbeef", "2024-01-01", "2024-01-31", SourceApp, ImpressionDetail},
		},
		{
			name: "pubkey filter with group by app_id and day orders by day desc",
			filter: ImpressionFilter{
				AppPubkey: "deadbeef",
				From:      "2024-01-01",
				To:        "2024-01-31",
				GroupBy:   []string{"app_id", "day"},
			},
			wantSQL:  "SELECT app_id, day, SUM(count) AS count FROM app_impressions WHERE app_pubkey = ? AND day >= ? AND day <= ? GROUP BY app_id, day ORDER BY day DESC",
			wantArgs: []any{"deadbeef", "2024-01-01", "2024-01-31"},
		},
		{
			name:     "group by does not mutate caller slice",
			filter:   ImpressionFilter{From: "2024-01-01", To: "2024-01-31", GroupBy: []string{"app_id"}},
			wantSQL:  "SELECT app_id, SUM(count) AS count FROM app_impressions WHERE day >= ? AND day <= ? GROUP BY app_id",
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
				{Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: ImpressionDetail}, 1},
			},
			want: []ImpressionCount{
				{Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: ImpressionDetail}, 1},
			},
		},
		{
			name: "count is persisted correctly",
			batch: []ImpressionCount{
				{Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: ImpressionDetail}, 42},
			},
			want: []ImpressionCount{
				{Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: ImpressionDetail}, 42},
			},
		},
		{
			name: "multiple distinct impressions",
			batch: []ImpressionCount{
				{Impression{AppID: "com.example.app1", Day: "2024-01-01", Source: SourceApp, Type: ImpressionDetail}, 3},
				{Impression{AppID: "com.example.app2", Day: "2024-01-01", Source: SourceWeb, Type: ImpressionDetail}, 7},
				{Impression{AppID: "com.example.app1", Day: "2024-01-02", Source: SourceApp, Type: ImpressionDetail}, 1},
			},
			want: []ImpressionCount{
				{Impression{AppID: "com.example.app1", Day: "2024-01-01", Source: SourceApp, Type: ImpressionDetail}, 3},
				{Impression{AppID: "com.example.app2", Day: "2024-01-01", Source: SourceWeb, Type: ImpressionDetail}, 7},
				{Impression{AppID: "com.example.app1", Day: "2024-01-02", Source: SourceApp, Type: ImpressionDetail}, 1},
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

			if err := s.SaveImpressions(ctx, test.batch); err != nil {
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

	imp := Impression{AppID: "com.example.app", Day: "2024-01-01", Source: SourceApp, Type: ImpressionDetail}

	if err := s.SaveImpressions(ctx, []ImpressionCount{{imp, 3}}); err != nil {
		t.Fatalf("first SaveImpressions: %v", err)
	}
	if err := s.SaveImpressions(ctx, []ImpressionCount{{imp, 5}}); err != nil {
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
	rows, err := db.Query(`SELECT app_id, app_pubkey, day, source, type, country_code, count FROM app_impressions`)
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
				Type:        ImpressionType(typ),
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
