package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/pippellia-btc/blossom"
)

var ctx = context.Background()

// --- queryDownloadsSQL ---

func TestQueryDownloadsSQL(t *testing.T) {
	h := blossom.ComputeHash([]byte("anything"))
	hStr := h.Hex()

	tests := []struct {
		name     string
		filter   DownloadFilter
		wantSQL  string
		wantArgs []any
	}{
		{
			name:     "no filters no group by",
			filter:   DownloadFilter{},
			wantSQL:  "SELECT SUM(count) AS count FROM downloads",
			wantArgs: nil,
		},
		{
			name:     "from and to filters",
			filter:   DownloadFilter{From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM downloads WHERE day >= ? AND day <= ?",
			wantArgs: []any{"2024-01-01", "2024-01-31"},
		},
		{
			name:     "hash filter",
			filter:   DownloadFilter{Hash: hStr, From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM downloads WHERE hash = ? AND day >= ? AND day <= ?",
			wantArgs: []any{hStr, "2024-01-01", "2024-01-31"},
		},
		{
			name:     "source filter",
			filter:   DownloadFilter{Source: SourceApp, From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM downloads WHERE day >= ? AND day <= ? AND source = ?",
			wantArgs: []any{"2024-01-01", "2024-01-31", SourceApp},
		},
		{
			name:     "group by hash emits SELECT col and GROUP BY clause",
			filter:   DownloadFilter{From: "2024-01-01", To: "2024-01-31", GroupBy: []string{"hash"}},
			wantSQL:  "SELECT hash, SUM(count) AS count FROM downloads WHERE day >= ? AND day <= ? GROUP BY hash",
			wantArgs: []any{"2024-01-01", "2024-01-31"},
		},
		{
			name:     "group by day emits ORDER BY day DESC",
			filter:   DownloadFilter{From: "2024-01-01", To: "2024-01-31", GroupBy: []string{"day"}},
			wantSQL:  "SELECT day, SUM(count) AS count FROM downloads WHERE day >= ? AND day <= ? GROUP BY day ORDER BY day DESC",
			wantArgs: []any{"2024-01-01", "2024-01-31"},
		},
		{
			name:     "group by country_code",
			filter:   DownloadFilter{From: "2024-01-01", To: "2024-01-31", GroupBy: []string{"country_code"}},
			wantSQL:  "SELECT country_code, SUM(count) AS count FROM downloads WHERE day >= ? AND day <= ? GROUP BY country_code",
			wantArgs: []any{"2024-01-01", "2024-01-31"},
		},
		{
			name: "all filters with group by hash and day",
			filter: DownloadFilter{
				Hash:    hStr,
				From:    "2024-01-01",
				To:      "2024-01-31",
				Source:  SourceWeb,
				GroupBy: []string{"hash", "day"},
			},
			wantSQL:  "SELECT hash, day, SUM(count) AS count FROM downloads WHERE hash = ? AND day >= ? AND day <= ? AND source = ? GROUP BY hash, day ORDER BY day DESC",
			wantArgs: []any{hStr, "2024-01-01", "2024-01-31", SourceWeb},
		},
		{
			name:     "group by does not mutate caller slice",
			filter:   DownloadFilter{From: "2024-01-01", To: "2024-01-31", GroupBy: []string{"hash"}},
			wantSQL:  "SELECT hash, SUM(count) AS count FROM downloads WHERE day >= ? AND day <= ? GROUP BY hash",
			wantArgs: []any{"2024-01-01", "2024-01-31"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			origGroupBy := make([]string, len(test.filter.GroupBy))
			copy(origGroupBy, test.filter.GroupBy)

			gotSQL, gotArgs := queryDownloadsSQL(test.filter)

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

func TestSaveDownloads(t *testing.T) {
	h1 := blossom.ComputeHash([]byte("anything"))
	h2 := blossom.ComputeHash([]byte("anywhere"))

	tests := []struct {
		name  string
		batch []DownloadCount
		want  []DownloadCount
	}{
		{
			name:  "empty batch is a no-op",
			batch: nil,
			want:  nil,
		},
		{
			name: "single download",
			batch: []DownloadCount{
				{Download{Hash: h1, Day: "2024-01-01", Source: SourceApp}, 1},
			},
			want: []DownloadCount{
				{Download{Hash: h1, Day: "2024-01-01", Source: SourceApp}, 1},
			},
		},
		{
			name: "count is persisted correctly",
			batch: []DownloadCount{
				{Download{Hash: h1, Day: "2024-01-01", Source: SourceApp}, 42},
			},
			want: []DownloadCount{
				{Download{Hash: h1, Day: "2024-01-01", Source: SourceApp}, 42},
			},
		},
		{
			name: "multiple distinct downloads",
			batch: []DownloadCount{
				{Download{Hash: h1, Day: "2024-01-01", Source: SourceApp}, 3},
				{Download{Hash: h2, Day: "2024-01-01", Source: SourceApp}, 7},
				{Download{Hash: h1, Day: "2024-01-01", Source: SourceUnknown}, 1},
			},
			want: []DownloadCount{
				{Download{Hash: h1, Day: "2024-01-01", Source: SourceApp}, 3},
				{Download{Hash: h2, Day: "2024-01-01", Source: SourceApp}, 7},
				{Download{Hash: h1, Day: "2024-01-01", Source: SourceUnknown}, 1},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := New(":memory:")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer store.Close()

			if err := store.SaveDownloads(ctx, test.batch); err != nil {
				t.Fatalf("SaveDownloads: %v", err)
			}

			got, err := queryDownloads(store.db)
			if err != nil {
				t.Fatalf("queryDownloads: %v", err)
			}

			sortDownloadCounts(got)
			sortDownloadCounts(test.want)

			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("mismatch\n got: %v\nwant: %v", got, test.want)
			}
		})
	}
}

func TestSaveDownloads_AccumulatesAcrossCalls(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	dl := Download{Hash: blossom.ComputeHash([]byte("anything")), Day: "2024-01-01", Source: SourceApp}

	if err := s.SaveDownloads(ctx, []DownloadCount{{dl, 3}}); err != nil {
		t.Fatalf("first SaveDownloads: %v", err)
	}
	if err := s.SaveDownloads(ctx, []DownloadCount{{dl, 5}}); err != nil {
		t.Fatalf("second SaveDownloads: %v", err)
	}

	got, err := queryDownloads(s.db)
	if err != nil {
		t.Fatalf("queryDownloads: %v", err)
	}

	want := []DownloadCount{{dl, 8}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mismatch\n got: %v\nwant: %v", got, want)
	}
}

// --- Helpers ---

func queryDownloads(db *sql.DB) ([]DownloadCount, error) {
	rows, err := db.Query(`SELECT hash, day, source, country_code, count FROM downloads`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []DownloadCount
	for rows.Next() {
		var (
			hash                     blossom.Hash
			day, source, countryCode string
			count                    int
		)
		if err := rows.Scan(&hash, &day, &source, &countryCode, &count); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		results = append(results, DownloadCount{
			Download: Download{
				Hash:        hash,
				Day:         normalizeDay(day),
				Source:      Source(source),
				CountryCode: countryCode,
			},
			Count: count,
		})
	}
	return results, rows.Err()
}

func sortDownloadCounts(rows []DownloadCount) {
	slices.SortFunc(rows, func(a, b DownloadCount) int {
		if c := cmp.Compare(a.Hash.Hex(), b.Hash.Hex()); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Day, b.Day); c != 0 {
			return c
		}
		return cmp.Compare(string(a.Source), string(b.Source))
	})
}
