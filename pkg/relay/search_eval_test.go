package relay

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zapstore/relay/pkg/relay/store"
	"github.com/zapstore/relay/pkg/search"
	"github.com/zapstore/steroid/artifacts"
	"github.com/zapstore/steroid/encode"
)

// evalCatalogKey signs the production catalog and its stacks.
const evalCatalogKey = "44dc0ab0606436c14454d7ecf3581b7b4f1b48857fd9851896a07dcf2b72b2bb"

// The floors a ranking change must not fall under. Raise them when the
// ranking improves. The numbers are measured on tests/fixtures/data.
const (
	evalFloorHit3 = 0.75
	evalFloorMRR  = 0.67
)

type evalCase struct {
	Query    string   `json:"query"`
	Relevant []string `json:"relevant"`
}

// TestSearchEval ranks the labeled queries in tests/fixtures/search-eval.json
// against the production catalog in tests/fixtures/data, with the real encoder
// and the loader the relay serves from. It models a typed query with no fact
// filter, as the apps send it. It skips when `make pull-data` has not run.
// Run it with -v to see the numbers and every query that misses.
func TestSearchEval(t *testing.T) {
	fixtures := filepath.Join("..", "..", "..", "tests", "fixtures")
	relayDB := filepath.Join(fixtures, "data", "relay.db")
	modelDir := filepath.Join(fixtures, "models", "leaf-ir-v1")
	for _, path := range []string{relayDB, artifacts.Path(filepath.Join(fixtures, "data")), modelDir} {
		if _, err := os.Stat(path); err != nil {
			t.Skipf("search eval needs %s: %v", path, err)
		}
	}
	var labels struct{ Cases []evalCase }
	raw, err := os.ReadFile(filepath.Join(fixtures, "search-eval.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &labels); err != nil {
		t.Fatal(err)
	}

	// The store applies its schema on open, so it reads a copy.
	db, err := store.New(copyFile(t, relayDB))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	entries, err := SearchListings(db, evalCatalogKey, artifacts.Path(filepath.Join(fixtures, "data")))(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	apps := make([]search.App, len(entries))
	for i, entry := range entries {
		apps[i] = entry.App
	}
	encoder, err := encode.Open(t.Context(), modelDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { encoder.Close() })

	var hit1, hit3, reciprocal, precision5 float64
	for _, c := range labels.Cases {
		query := search.Normalize(c.Query)
		vector, err := encoder.Query(t.Context(), query)
		if err != nil {
			t.Fatal(err)
		}
		ranked := search.Rank(apps, query, vector, 0, search.Limit, 0, false, 0, 0)
		first, found := -1, 0
		for i, app := range ranked {
			if slices.Contains(c.Relevant, app.ID) {
				if first < 0 {
					first = i
				}
				if i < 5 {
					found++
				}
			}
		}
		if first == 0 {
			hit1++
		}
		if first >= 0 && first < 3 {
			hit3++
		}
		if first >= 0 {
			reciprocal += 1 / float64(first+1)
		}
		precision5 += float64(found) / 5
		if first < 0 || first >= 3 {
			top := make([]string, 0, 4)
			for _, app := range ranked[:min(4, len(ranked))] {
				top = append(top, app.Name)
			}
			t.Logf("miss %q: %v", c.Query, top)
		}
	}
	n := float64(len(labels.Cases))
	t.Logf("search eval over %d apps, %d queries: hit@1=%.3f hit@3=%.3f mrr=%.3f p@5=%.3f",
		len(apps), len(labels.Cases), hit1/n, hit3/n, reciprocal/n, precision5/n)
	if hit3/n < evalFloorHit3 || reciprocal/n < evalFloorMRR {
		t.Errorf("ranking fell under the floors: hit@3 %.3f (floor %.2f), mrr %.3f (floor %.2f)",
			hit3/n, evalFloorHit3, reciprocal/n, evalFloorMRR)
	}
}

func copyFile(t *testing.T, from string) string {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	to := filepath.Join(t.TempDir(), "relay.db")
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	return to
}
