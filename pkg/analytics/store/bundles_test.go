package store

import (
	"testing"
)

func TestSaveBundlesAccumulates(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	row := BundleCount{BundleRequest: BundleRequest{
		Day: "2026-10-17", FromEpoch: 2, ToEpoch: 5, Status: 200, Protocol: "v1", ClientVersion: "2.0.1",
	}, Count: 3}
	if err := s.SaveBundles(ctx, []BundleCount{row}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	row.Count = 2
	if err := s.SaveBundles(ctx, []BundleCount{row}); err != nil {
		t.Fatalf("second save: %v", err)
	}

	got, err := s.QueryBundles(ctx, BundleFilter{
		GroupBy: []string{"day", "from_epoch", "to_epoch", "status", "protocol", "client_version"},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows %d: %+v", len(got), got)
	}
	want := row.BundleRequest
	if got[0].BundleRequest != want || got[0].Count != 5 {
		t.Fatalf("got %+v, want %+v count 5", got[0], want)
	}
}

func TestQueryBundlesFiltersEpochAndClient(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	batch := []BundleCount{
		{BundleRequest: BundleRequest{Day: "2026-10-17", FromEpoch: 0, ToEpoch: 5, Status: 200, Protocol: "v1", ClientVersion: "2.0.1"}, Count: 4},
		{BundleRequest: BundleRequest{Day: "2026-10-17", FromEpoch: 5, ToEpoch: 5, Status: 304, Protocol: "v1", ClientVersion: "1.0"}, Count: 9},
		{BundleRequest: BundleRequest{Day: "2026-10-17", FromEpoch: 2, ToEpoch: UnknownEpoch, Status: 400, Protocol: "v2", ClientVersion: "9.9.9"}, Count: 1},
	}
	if err := s.SaveBundles(ctx, batch); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := s.QueryBundles(ctx, BundleFilter{FromEpoch: "0", ClientVersion: "2.0.1", Status: "200"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].Count != 4 {
		t.Fatalf("got %+v", got)
	}

	if _, err := s.QueryBundles(ctx, BundleFilter{Status: "nope"}); err == nil {
		t.Fatal("invalid status accepted")
	}
}
