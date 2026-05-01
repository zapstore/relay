package store

import (
	"cmp"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/pippellia-btc/blossom"
)

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
			wantSQL:  "SELECT SUM(count) AS count FROM app_downloads",
			wantArgs: nil,
		},
		{
			name:     "from and to filters",
			filter:   DownloadFilter{From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM app_downloads WHERE day >= ? AND day <= ?",
			wantArgs: []any{"2024-01-01", "2024-01-31"},
		},
		{
			name:     "hash filter",
			filter:   DownloadFilter{Hash: hStr, From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM app_downloads WHERE hash = ? AND day >= ? AND day <= ?",
			wantArgs: []any{hStr, "2024-01-01", "2024-01-31"},
		},
		{
			name:     "source filter",
			filter:   DownloadFilter{Source: SourceApp, From: "2024-01-01", To: "2024-01-31"},
			wantSQL:  "SELECT SUM(count) AS count FROM app_downloads WHERE day >= ? AND day <= ? AND source = ?",
			wantArgs: []any{"2024-01-01", "2024-01-31", SourceApp},
		},
		{
			name:     "group by hash emits SELECT col and GROUP BY clause",
			filter:   DownloadFilter{From: "2024-01-01", To: "2024-01-31", GroupBy: []string{"hash"}},
			wantSQL:  "SELECT hash, SUM(count) AS count FROM app_downloads WHERE day >= ? AND day <= ? GROUP BY hash",
			wantArgs: []any{"2024-01-01", "2024-01-31"},
		},
		{
			name:     "group by day emits ORDER BY day DESC",
			filter:   DownloadFilter{From: "2024-01-01", To: "2024-01-31", GroupBy: []string{"day"}},
			wantSQL:  "SELECT day, SUM(count) AS count FROM app_downloads WHERE day >= ? AND day <= ? GROUP BY day ORDER BY day DESC",
			wantArgs: []any{"2024-01-01", "2024-01-31"},
		},
		{
			name:     "group by country_code",
			filter:   DownloadFilter{From: "2024-01-01", To: "2024-01-31", GroupBy: []string{"country_code"}},
			wantSQL:  "SELECT country_code, SUM(count) AS count FROM app_downloads WHERE day >= ? AND day <= ? GROUP BY country_code",
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
			wantSQL:  "SELECT hash, day, SUM(count) AS count FROM app_downloads WHERE hash = ? AND day >= ? AND day <= ? AND source = ? GROUP BY hash, day ORDER BY day DESC",
			wantArgs: []any{hStr, "2024-01-01", "2024-01-31", SourceWeb},
		},
		{
			name:     "group by does not mutate caller slice",
			filter:   DownloadFilter{From: "2024-01-01", To: "2024-01-31", GroupBy: []string{"hash"}},
			wantSQL:  "SELECT hash, SUM(count) AS count FROM app_downloads WHERE day >= ? AND day <= ? GROUP BY hash",
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
				{Download{Hash: h1, Type: Install, Day: "2024-01-01", Source: SourceApp}, 1},
			},
			want: []DownloadCount{
				{Download{Hash: h1, Type: Install, Day: "2024-01-01", Source: SourceApp}, 1},
			},
		},
		{
			name: "count is persisted correctly",
			batch: []DownloadCount{
				{Download{Hash: h1, Type: Install, Day: "2024-01-01", Source: SourceApp}, 42},
			},
			want: []DownloadCount{
				{Download{Hash: h1, Type: Install, Day: "2024-01-01", Source: SourceApp}, 42},
			},
		},
		{
			name: "multiple distinct downloads",
			batch: []DownloadCount{
				{Download{Hash: h1, Type: Install, Day: "2024-01-01", Source: SourceApp}, 3},
				{Download{Hash: h2, Type: Install, Day: "2024-01-01", Source: SourceApp}, 7},
				{Download{Hash: h1, Type: Update, Day: "2024-01-01", Source: SourceUnknown}, 1},
			},
			want: []DownloadCount{
				{Download{Hash: h1, Type: Install, Day: "2024-01-01", Source: SourceApp}, 3},
				{Download{Hash: h2, Type: Install, Day: "2024-01-01", Source: SourceApp}, 7},
				{Download{Hash: h1, Type: Update, Day: "2024-01-01", Source: SourceUnknown}, 1},
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

			got, err := allDownloads(store.db)
			if err != nil {
				t.Fatalf("allDownloads: %v", err)
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

	dl := Download{
		Hash:   blossom.ComputeHash([]byte("anything")),
		Day:    "2024-01-01",
		Source: SourceApp,
		Type:   Install,
	}

	if err := s.SaveDownloads(ctx, []DownloadCount{{dl, 3}}); err != nil {
		t.Fatalf("first SaveDownloads: %v", err)
	}
	if err := s.SaveDownloads(ctx, []DownloadCount{{dl, 5}}); err != nil {
		t.Fatalf("second SaveDownloads: %v", err)
	}

	got, err := allDownloads(s.db)
	if err != nil {
		t.Fatalf("allDownloads: %v", err)
	}

	want := []DownloadCount{{dl, 8}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mismatch\n got: %v\nwant: %v", got, want)
	}
}

// --- QueryDownloads ---

func TestQueryDownloads(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	h1 := blossom.ComputeHash([]byte("file1"))
	h2 := blossom.ComputeHash([]byte("file2"))

	seed := []DownloadCount{
		{Download{Hash: h1, Day: "2024-01-01", Source: SourceApp, Type: Install, CountryCode: "US"}, 20},
		{Download{Hash: h1, Day: "2024-01-02", Source: SourceApp, Type: Install, CountryCode: "US"}, 8},
		{Download{Hash: h1, Day: "2024-01-03", Source: SourceWeb, Type: Install, CountryCode: "DE"}, 4},
		{Download{Hash: h2, Day: "2024-01-01", Source: SourceApp, Type: Update, CountryCode: "US"}, 6},
		{Download{Hash: h2, Day: "2024-01-02", Source: SourceUnknown, Type: Update, CountryCode: "FR"}, 2},
	}
	if err := s.SaveDownloads(ctx, seed); err != nil {
		t.Fatalf("SaveDownloads: %v", err)
	}

	t.Run("total count no filters", func(t *testing.T) {
		got, err := s.QueryDownloads(ctx, DownloadFilter{})
		if err != nil {
			t.Fatalf("QueryDownloads: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 row, got %d", len(got))
		}
		if got[0].Count != 40 {
			t.Errorf("got count %d, want 40", got[0].Count)
		}
	})

	t.Run("filter by date range", func(t *testing.T) {
		got, err := s.QueryDownloads(ctx, DownloadFilter{
			From: "2024-01-01",
			To:   "2024-01-02",
		})
		if err != nil {
			t.Fatalf("QueryDownloads: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 row, got %d", len(got))
		}
		if got[0].Count != 36 {
			t.Errorf("got count %d, want 36", got[0].Count)
		}
	})

	t.Run("filter by hash", func(t *testing.T) {
		got, err := s.QueryDownloads(ctx, DownloadFilter{Hash: h1.Hex()})
		if err != nil {
			t.Fatalf("QueryDownloads: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 row, got %d", len(got))
		}
		if got[0].Count != 32 {
			t.Errorf("got count %d, want 32", got[0].Count)
		}
	})

	t.Run("filter by source", func(t *testing.T) {
		got, err := s.QueryDownloads(ctx, DownloadFilter{Source: SourceApp})
		if err != nil {
			t.Fatalf("QueryDownloads: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 row, got %d", len(got))
		}
		if got[0].Count != 34 {
			t.Errorf("got count %d, want 34", got[0].Count)
		}
	})

	t.Run("filter by type", func(t *testing.T) {
		got, err := s.QueryDownloads(ctx, DownloadFilter{Type: Install})
		if err != nil {
			t.Fatalf("QueryDownloads: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 row, got %d", len(got))
		}
		if got[0].Count != 32 {
			t.Errorf("got count %d, want 32", got[0].Count)
		}
	})

	t.Run("group by day ordered desc", func(t *testing.T) {
		got, err := s.QueryDownloads(ctx, DownloadFilter{GroupBy: []string{"day"}})
		if err != nil {
			t.Fatalf("QueryDownloads: %v", err)
		}
		want := []DownloadCount{
			{Download{Day: "2024-01-03"}, 4},
			{Download{Day: "2024-01-02"}, 10},
			{Download{Day: "2024-01-01"}, 26},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("group by hash", func(t *testing.T) {
		got, err := s.QueryDownloads(ctx, DownloadFilter{GroupBy: []string{"hash"}})
		if err != nil {
			t.Fatalf("QueryDownloads: %v", err)
		}
		want := []DownloadCount{
			{Download{Hash: h1}, 32},
			{Download{Hash: h2}, 8},
		}
		sortDownloadCounts(got)
		sortDownloadCounts(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

// --- Helpers ---

func allDownloads(db *sql.DB) ([]DownloadCount, error) {
	rows, err := db.Query(`SELECT hash, app_id, app_version, app_pubkey, day, source, type, country_code, count FROM app_downloads`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []DownloadCount
	for rows.Next() {
		var (
			hash                          blossom.Hash
			appID, appVersion, appPubkey  string
			day, source, typ, countryCode string
			count                         int
		)
		if err := rows.Scan(&hash, &appID, &appVersion, &appPubkey, &day, &source, &typ, &countryCode, &count); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		results = append(results, DownloadCount{
			Download: Download{
				Hash:        hash,
				AppID:       appID,
				AppVersion:  appVersion,
				AppPubkey:   appPubkey,
				Day:         normalizeDay(day),
				Source:      Source(source),
				Type:        DownloadType(typ),
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
