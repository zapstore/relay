package search

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

type fixture struct {
	Vectors map[string]struct{ At, Len, Value int }
	Apps    []struct {
		ID       string
		Name     string
		Facts    []string
		Keywords []string
		Vector   string
	}
	Cases []struct {
		Query    string
		Vector   string
		Facts    []string
		Filters  []string
		Relevant []string
		Top      []string
	}
}

var fixtureBits = map[string]int{
	"open_source":      FactSourceAvailable,
	"source_available": FactSourceAvailable,
	"google_services":  FactGoogle,
	"offline_capable":  FactOffline,
	"e2ee":             FactE2EE,
}

func TestSharedFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "fixtures", "search.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	vector := func(name string) []int8 {
		if name == "" {
			return nil
		}
		spec, ok := f.Vectors[name]
		if !ok {
			t.Fatalf("missing vector %s", name)
		}
		out := make([]int8, VectorDims)
		for i := 0; i < spec.Len; i++ {
			out[spec.At+i] = int8(spec.Value)
		}
		return out
	}
	mask := func(names []string) int {
		m := 0
		for _, name := range names {
			m |= fixtureBits[name]
		}
		return m
	}
	var apps []App
	for _, a := range f.Apps {
		apps = append(apps, App{ID: a.ID, Name: a.Name, Vector: vector(a.Vector), FactBits: mask(a.Facts), Keywords: a.Keywords})
	}
	for _, c := range f.Cases {
		parsed := Parse(Normalize(c.Query))
		if got := parsed.Boost | parsed.Penalty; got != mask(c.Facts) {
			t.Errorf("%q facts = %b, want %b", c.Query, got, mask(c.Facts))
		}
		ranked := Rank(apps, parsed.Residual, vector(c.Vector), parsed.Boost, 100, parsed.Penalty, false, mask(c.Filters)|parsed.Hard, parsed.Exclude)
		ids := make([]string, 0, len(ranked))
		for _, app := range ranked {
			ids = append(ids, app.ID)
		}
		if !slices.Equal(ids, c.Top) && !(len(ids) == 0 && len(c.Top) == 0) {
			t.Errorf("%q = %v, want %v", c.Query, ids, c.Top)
		}
	}
}

func TestParseSecurity(t *testing.T) {
	got := ParseSecurity(`{"facts":{"source_available":true,"google_services":false,"offline_capable":true,"tracking":true},"keywords":["nostr", 3]}`)
	if got.FactBits != FactSourceAvailable|FactOffline || !slices.Equal(got.Keywords, []string{"nostr"}) {
		t.Fatalf("%+v", got)
	}
	legacy := ParseSecurity("\"e2ee\",\"yes\",\"\"\n\"google_services\",\"no\",\"\"\n---\nbody\n---\n")
	if legacy.FactBits != FactE2EE {
		t.Fatalf("legacy %+v", legacy)
	}
	if ParseSecurity("not a file").FactBits != 0 {
		t.Fatal("text without facts set bits")
	}
}
