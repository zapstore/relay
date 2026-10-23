package analytics

import (
	"path/filepath"
	"testing"

	"github.com/zapstore/relay/pkg/analytics/store"
)

func TestRecordBundleFlushesOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analytics.db")
	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	cfg := NewConfig()
	engine := NewEngine(cfg, db)
	engine.RecordBundle(2, 5, 200, "v1", "2.0.1")
	engine.RecordBundle(2, 5, 200, "v1", "2.0.1")
	engine.Close()

	db, err = NewDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryBundles(t.Context(), store.BundleFilter{
		GroupBy: []string{"day", "from_epoch", "to_epoch", "status", "protocol", "client_version"},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows %d: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.FromEpoch != 2 || got.ToEpoch != 5 || got.Status != 200 || got.Protocol != "v1" || got.ClientVersion != "2.0.1" || got.Count != 2 {
		t.Fatalf("got %+v", got)
	}
	if got.Day != store.Today() {
		t.Fatalf("day %s, want %s", got.Day, store.Today())
	}
}
