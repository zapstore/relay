package store

import (
	"database/sql"
	"fmt"
	"reflect"
	"testing"
)

// --- SaveRelayMetrics ---

func TestSaveRelayMetrics(t *testing.T) {
	tests := []struct {
		name    string
		metrics RelayMetrics
		want    RelayMetrics
	}{
		{
			name:    "all counters are persisted",
			metrics: RelayMetrics{Day: "2024-01-01", Reqs: 100, Filters: 250, Events: 75},
			want:    RelayMetrics{Day: "2024-01-01", Reqs: 100, Filters: 250, Events: 75},
		},
		{
			name:    "different day",
			metrics: RelayMetrics{Day: "2024-06-15", Reqs: 42, Filters: 84, Events: 21},
			want:    RelayMetrics{Day: "2024-06-15", Reqs: 42, Filters: 84, Events: 21},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, err := New(":memory:")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer s.Close()

			if err := s.SaveRelayMetrics(ctx, test.metrics); err != nil {
				t.Fatalf("SaveRelayMetrics: %v", err)
			}

			got, err := queryRelayMetrics(s.db, test.metrics.Day)
			if err != nil {
				t.Fatalf("queryRelayMetrics: %v", err)
			}

			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("mismatch\n got: %+v\nwant: %+v", got, test.want)
			}
		})
	}
}

func TestSaveRelayMetrics_AccumulatesAcrossCalls(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	if err := s.SaveRelayMetrics(ctx, RelayMetrics{Day: "2024-01-01", Reqs: 10, Filters: 20, Events: 5}); err != nil {
		t.Fatalf("first SaveRelayMetrics: %v", err)
	}
	if err := s.SaveRelayMetrics(ctx, RelayMetrics{Day: "2024-01-01", Reqs: 3, Filters: 7, Events: 2}); err != nil {
		t.Fatalf("second SaveRelayMetrics: %v", err)
	}

	got, err := queryRelayMetrics(s.db, "2024-01-01")
	if err != nil {
		t.Fatalf("queryRelayMetrics: %v", err)
	}

	want := RelayMetrics{Day: "2024-01-01", Reqs: 13, Filters: 27, Events: 7}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestSaveRelayMetrics_DifferentDaysAreIndependent(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	m1 := RelayMetrics{Day: "2024-01-01", Reqs: 10, Filters: 20, Events: 5}
	m2 := RelayMetrics{Day: "2024-01-02", Reqs: 30, Filters: 60, Events: 15}

	if err := s.SaveRelayMetrics(ctx, m1); err != nil {
		t.Fatalf("SaveRelayMetrics day 1: %v", err)
	}
	if err := s.SaveRelayMetrics(ctx, m2); err != nil {
		t.Fatalf("SaveRelayMetrics day 2: %v", err)
	}

	got1, err := queryRelayMetrics(s.db, "2024-01-01")
	if err != nil {
		t.Fatalf("queryRelayMetrics day 1: %v", err)
	}
	got2, err := queryRelayMetrics(s.db, "2024-01-02")
	if err != nil {
		t.Fatalf("queryRelayMetrics day 2: %v", err)
	}

	if !reflect.DeepEqual(got1, m1) {
		t.Fatalf("day 1 mismatch\n got: %+v\nwant: %+v", got1, m1)
	}
	if !reflect.DeepEqual(got2, m2) {
		t.Fatalf("day 2 mismatch\n got: %+v\nwant: %+v", got2, m2)
	}
}

// --- SaveBlossomMetrics ---

func TestSaveBlossomMetrics(t *testing.T) {
	tests := []struct {
		name    string
		metrics BlossomMetrics
		want    BlossomMetrics
	}{
		{
			name:    "all counters are persisted",
			metrics: BlossomMetrics{Day: "2024-01-01", Uploads: 50, Downloads: 300, Checks: 120},
			want:    BlossomMetrics{Day: "2024-01-01", Uploads: 50, Downloads: 300, Checks: 120},
		},
		{
			name:    "different day",
			metrics: BlossomMetrics{Day: "2024-06-15", Uploads: 1, Downloads: 2, Checks: 3},
			want:    BlossomMetrics{Day: "2024-06-15", Uploads: 1, Downloads: 2, Checks: 3},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, err := New(":memory:")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer s.Close()

			if err := s.SaveBlossomMetrics(ctx, test.metrics); err != nil {
				t.Fatalf("SaveBlossomMetrics: %v", err)
			}

			got, err := queryBlossomMetrics(s.db, test.metrics.Day)
			if err != nil {
				t.Fatalf("queryBlossomMetrics: %v", err)
			}

			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("mismatch\n got: %+v\nwant: %+v", got, test.want)
			}
		})
	}
}

func TestSaveBlossomMetrics_AccumulatesAcrossCalls(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	if err := s.SaveBlossomMetrics(ctx, BlossomMetrics{Day: "2024-01-01", Uploads: 10, Downloads: 50, Checks: 20}); err != nil {
		t.Fatalf("first SaveBlossomMetrics: %v", err)
	}
	if err := s.SaveBlossomMetrics(ctx, BlossomMetrics{Day: "2024-01-01", Uploads: 5, Downloads: 25, Checks: 10}); err != nil {
		t.Fatalf("second SaveBlossomMetrics: %v", err)
	}

	got, err := queryBlossomMetrics(s.db, "2024-01-01")
	if err != nil {
		t.Fatalf("queryBlossomMetrics: %v", err)
	}

	want := BlossomMetrics{Day: "2024-01-01", Uploads: 15, Downloads: 75, Checks: 30}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestSaveBlossomMetrics_DifferentDaysAreIndependent(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	m1 := BlossomMetrics{Day: "2024-01-01", Uploads: 10, Downloads: 50, Checks: 20}
	m2 := BlossomMetrics{Day: "2024-01-02", Uploads: 30, Downloads: 150, Checks: 60}

	if err := s.SaveBlossomMetrics(ctx, m1); err != nil {
		t.Fatalf("SaveBlossomMetrics day 1: %v", err)
	}
	if err := s.SaveBlossomMetrics(ctx, m2); err != nil {
		t.Fatalf("SaveBlossomMetrics day 2: %v", err)
	}

	got1, err := queryBlossomMetrics(s.db, "2024-01-01")
	if err != nil {
		t.Fatalf("queryBlossomMetrics day 1: %v", err)
	}
	got2, err := queryBlossomMetrics(s.db, "2024-01-02")
	if err != nil {
		t.Fatalf("queryBlossomMetrics day 2: %v", err)
	}

	if !reflect.DeepEqual(got1, m1) {
		t.Fatalf("day 1 mismatch\n got: %+v\nwant: %+v", got1, m1)
	}
	if !reflect.DeepEqual(got2, m2) {
		t.Fatalf("day 2 mismatch\n got: %+v\nwant: %+v", got2, m2)
	}
}

// --- Helpers ---

func queryRelayMetrics(db *sql.DB, day string) (RelayMetrics, error) {
	var m RelayMetrics
	err := db.QueryRow(`
		SELECT day, reqs, filters, events
		FROM relay_metrics
		WHERE day = ?
	`, day).Scan(&m.Day, &m.Reqs, &m.Filters, &m.Events)
	if err != nil {
		return RelayMetrics{}, fmt.Errorf("scan relay_metrics: %w", err)
	}
	m.Day = normalizeDay(m.Day)
	return m, nil
}

func queryBlossomMetrics(db *sql.DB, day string) (BlossomMetrics, error) {
	var m BlossomMetrics
	err := db.QueryRow(`
		SELECT day, checks, downloads, uploads
		FROM blossom_metrics
		WHERE day = ?
	`, day).Scan(&m.Day, &m.Checks, &m.Downloads, &m.Uploads)
	if err != nil {
		return BlossomMetrics{}, fmt.Errorf("scan blossom_metrics: %w", err)
	}
	m.Day = normalizeDay(m.Day)
	return m, nil
}
