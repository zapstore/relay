package policy

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zapstore/relay/pkg/events"
)

const (
	catalogKey = "78ce6faa72264387284e647ba6938995735ec8c7d5c5a65737e55130f026307d"
	devKey     = "726a1e261cc6474674e8285e3951b3bb139be9a773d1acf49dc868db861a1c11"
	paidKey    = "f683e87035f7ad4f44e0b98cfbd9537e16455a92cd38cefc4cb31db7557f5ef2"
)

// fakeWoT allows the keys in scores whose score is at least 0.5.
type fakeWoT struct {
	scores map[string]float64
	err    error
	calls  int
}

func (f *fakeWoT) Allow(_ context.Context, pubkey string) (bool, float64, error) {
	f.calls++
	if f.err != nil {
		return false, 0, f.err
	}
	score := f.scores[pubkey]
	return score >= 0.5, score, nil
}

func TestGateAdmit(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	wot := &fakeWoT{scores: map[string]float64{devKey: 0.9}}
	gate := NewGate(store, strings.ToUpper(catalogKey), wot)

	tests := []struct {
		name   string
		pubkey string
		want   error
	}{
		{"catalog key", catalogKey, nil},
		{"unknown key", paidKey, ErrNotAdmitted},
		{"trusted developer", devKey, nil},
		{"stranger", "ab" + devKey[2:], ErrNotAdmitted},
		{"invalid pubkey", "nope", ErrNotAdmitted},
	}
	for _, test := range tests {
		err := gate.Admit(ctx, test.pubkey)
		if !errors.Is(err, test.want) {
			t.Fatalf("%s: got %v, want %v", test.name, err, test.want)
		}
	}
	// The catalog key and an invalid pubkey never reach the web of trust.
	if wot.calls != 3 {
		t.Fatalf("web of trust calls = %d, want 3", wot.calls)
	}
}

func TestGateRejectsWithoutWebOfTrust(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	gate := NewGate(store, catalogKey, nil)
	if err := gate.Admit(ctx, catalogKey); err != nil {
		t.Fatal(err)
	}
	if err := gate.Admit(ctx, paidKey); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("unknown key: %v", err)
	}
	if err := gate.Admit(ctx, devKey); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("developer: %v", err)
	}
	var nilGate *Gate
	if err := nilGate.Admit(ctx, devKey); err != nil {
		t.Fatalf("nil gate rejected: %v", err)
	}
}

func TestGateWebOfTrustFailureRejects(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	wot := &fakeWoT{err: errors.New("vertex down")}
	gate := NewGate(store, catalogKey, wot)
	if err := gate.Admit(ctx, paidKey); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("unknown key: %v", err)
	}
	if err := gate.Admit(ctx, devKey); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("developer: %v", err)
	}
	if wot.calls != 2 {
		t.Fatalf("web of trust calls = %d, want 2", wot.calls)
	}
}

func TestGateSkipsVertexAfterAppPublish(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	wot := &fakeWoT{scores: map[string]float64{devKey: 0.1}}
	gate := NewGate(store, catalogKey, wot)
	if _, err := store.db.Exec(`INSERT INTO events (id, pubkey, created_at, kind, tags, content, sig) VALUES ('rel', ?, 1, 30063, '[]', '', 'sig')`, devKey); err != nil {
		t.Fatal(err)
	}
	if err := gate.Admit(ctx, devKey); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("release only: %v", err)
	}
	if wot.calls != 1 {
		t.Fatalf("calls = %d, want 1", wot.calls)
	}
	if _, err := store.db.Exec(`INSERT INTO events (id, pubkey, created_at, kind, tags, content, sig) VALUES ('app', ?, 1, ?, '[]', '', 'sig')`, devKey, events.KindApp); err != nil {
		t.Fatal(err)
	}
	if err := gate.Admit(ctx, devKey); err != nil {
		t.Fatal(err)
	}
	if wot.calls != 1 {
		t.Fatalf("calls after app = %d, want 1", wot.calls)
	}
}

func TestOpenDropsPaidAndSuggestionTables(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE events (
		id TEXT PRIMARY KEY, pubkey TEXT NOT NULL, created_at INTEGER NOT NULL,
		kind INTEGER NOT NULL, tags TEXT NOT NULL, content TEXT NOT NULL, sig TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`CREATE TABLE allowed (kind TEXT NOT NULL, id TEXT NOT NULL, PRIMARY KEY (kind, id))`,
		`CREATE TABLE paid_access (kind TEXT NOT NULL, id TEXT NOT NULL, PRIMARY KEY (kind, id))`,
		`CREATE TABLE suggestions (id TEXT PRIMARY KEY)`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(db); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"allowed", "paid_access", "suggestions"} {
		var got string
		err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&got)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s still present: %v %q", name, err, got)
		}
	}
}
