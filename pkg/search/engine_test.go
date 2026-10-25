package search

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

func testEntries() []Entry {
	return []Entry{
		{App: App{ID: "org.example.notes", Name: "Notes"}, Event: nostr.Event{Kind: 32267, Tags: nostr.Tags{{"f", "android-arm64-v8a"}}}},
		{App: App{ID: "org.example.maps", Name: "Maps"}, Event: nostr.Event{Kind: 32267, Tags: nostr.Tags{{"f", "android-armeabi-v7a"}}}},
	}
}

func hitIDs(result Result) []string {
	ids := make([]string, len(result.Hits))
	for i, hit := range result.Hits {
		ids[i] = hit.ID
	}
	return ids
}

func TestEngineSearch(t *testing.T) {
	encodeCalls := 0
	failing := func(context.Context, string) ([]int8, error) {
		encodeCalls++
		return nil, errors.New("model gone")
	}
	engine := NewEngine(func(context.Context) ([]Entry, error) { return testEntries(), nil }, failing, time.Minute, slog.Default())

	result, err := engine.Search(t.Context(), Request{Query: "Notes"})
	if err != nil {
		t.Fatal(err)
	}
	if ids := hitIDs(result); len(ids) == 0 || ids[0] != "org.example.notes" {
		t.Fatalf("named app: %v", ids)
	}
	if encodeCalls != 1 {
		t.Fatalf("encoder calls = %d", encodeCalls)
	}

	result, err = engine.Search(t.Context(), Request{
		Query: "Notes",
		Match: func(e nostr.Event) bool { return e.Tags.ContainsAny("f", []string{"android-armeabi-v7a"}) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(hitIDs(result), "org.example.notes") {
		t.Fatal("Match did not filter before ranking")
	}

	if result, _ := engine.Search(t.Context(), Request{Query: "n"}); len(result.Hits) != 0 {
		t.Fatal("a one-letter query ranked")
	}
}

func TestExtensions(t *testing.T) {
	query, add, remove := Extensions("maps  Filter:source_available -filter:offline_capable filter:nonsense filter:e2ee e2ee:chat")
	if query != "maps e2ee:chat" {
		t.Errorf("query = %q", query)
	}
	if !slices.Equal(add, []string{"source_available"}) || !slices.Equal(remove, []string{"offline_capable"}) {
		t.Errorf("add %v remove %v", add, remove)
	}

	entries := []Entry{
		{App: App{ID: "org.example.maps", Name: "Maps", FactBits: FactOffline}},
		{App: App{ID: "org.example.atlas", Name: "Atlas maps"}},
	}
	engine := NewEngine(func(context.Context) ([]Entry, error) { return entries, nil }, nil, time.Minute, slog.Default())
	search := func(raw string) []string {
		query, add, remove := Extensions(raw)
		result, err := engine.Search(t.Context(), Request{Query: query, Add: add, Remove: remove})
		if err != nil {
			t.Fatal(err)
		}
		return hitIDs(result)
	}
	if ids := search("offline maps"); !slices.Equal(ids, []string{"org.example.maps"}) {
		t.Errorf("phrase filter: %v", ids)
	}
	if ids := search("offline maps -filter:offline_capable"); len(ids) != 2 {
		t.Errorf("removed phrase filter: %v", ids)
	}
	if ids := search("maps filter:offline_capable"); !slices.Equal(ids, []string{"org.example.maps"}) {
		t.Errorf("added filter: %v", ids)
	}
}

func TestEngineReload(t *testing.T) {
	loads := 0
	fail := false
	engine := NewEngine(func(context.Context) ([]Entry, error) {
		loads++
		if fail {
			return nil, errors.New("db gone")
		}
		return testEntries(), nil
	}, nil, time.Hour, slog.Default())

	for range 2 {
		if _, err := engine.Search(t.Context(), Request{Query: "Notes"}); err != nil {
			t.Fatal(err)
		}
	}
	if loads != 1 {
		t.Fatalf("loads = %d within the refresh interval", loads)
	}

	engine.refresh, fail = 0, true
	result, err := engine.Search(t.Context(), Request{Query: "Notes"})
	if err != nil || len(result.Hits) == 0 {
		t.Fatalf("a failed reload dropped the previous selection: %v %v", result.Hits, err)
	}

	empty := NewEngine(func(context.Context) ([]Entry, error) { return nil, errors.New("db gone") }, nil, time.Hour, slog.Default())
	if _, err := empty.Search(t.Context(), Request{Query: "Notes"}); err == nil {
		t.Fatal("first load failure was ignored")
	}
}
