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
	if err := store.Allow(ctx, Publisher{Kind: KindNostr, ID: paidKey}, "paid 2026-10"); err != nil {
		t.Fatal(err)
	}
	wot := &fakeWoT{scores: map[string]float64{devKey: 0.9}}
	gate := NewGate(store, strings.ToUpper(catalogKey), wot)

	tests := []struct {
		name   string
		pubkey string
		want   error
	}{
		{"catalog key", catalogKey, nil},
		{"paid publisher", paidKey, nil},
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
	// Catalog key and paid publisher never reach the web of trust.
	// Trusted developer and stranger do.
	if wot.calls != 2 {
		t.Fatalf("web of trust calls = %d, want 2", wot.calls)
	}
}

func TestGateRequiresPaymentWithoutWebOfTrust(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Allow(ctx, Publisher{Kind: KindNostr, ID: paidKey}, "paid"); err != nil {
		t.Fatal(err)
	}
	gate := NewGate(store, catalogKey, nil)
	if err := gate.Admit(ctx, catalogKey); err != nil {
		t.Fatal(err)
	}
	if err := gate.Admit(ctx, paidKey); err != nil {
		t.Fatal(err)
	}
	if err := gate.Admit(ctx, devKey); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("unpaid: %v", err)
	}
	var nilGate *Gate
	if err := nilGate.Admit(ctx, devKey); err != nil {
		t.Fatalf("nil gate rejected: %v", err)
	}
}

func TestGateWebOfTrustFailureRequiresPayment(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Allow(ctx, Publisher{Kind: KindNostr, ID: paidKey}, "paid"); err != nil {
		t.Fatal(err)
	}
	wot := &fakeWoT{err: errors.New("vertex down")}
	gate := NewGate(store, catalogKey, wot)
	if err := gate.Admit(ctx, paidKey); err != nil {
		t.Fatal(err)
	}
	if wot.calls != 0 {
		t.Fatalf("paid publisher called web of trust %d times", wot.calls)
	}
	if err := gate.Admit(ctx, devKey); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("unpaid: %v", err)
	}
}

func TestAllowedList(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	pub := Publisher{Kind: KindNostr, ID: paidKey}
	if err := store.Allow(ctx, pub, ""); !errors.Is(err, ErrReason) {
		t.Fatalf("empty reason: %v", err)
	}
	if err := store.Allow(ctx, pub, "paid"); err != nil {
		t.Fatal(err)
	}
	if err := store.Allow(ctx, pub, "paid again"); err != nil {
		t.Fatal(err)
	}
	rows, err := store.AllowedList(ctx)
	if err != nil || len(rows) != 1 || rows[0].Reason != "paid again" {
		t.Fatalf("rows %+v, %v", rows, err)
	}
	if err := store.Disallow(ctx, KindNostr, paidKey); err != nil {
		t.Fatal(err)
	}
	if err := store.Disallow(ctx, KindNostr, paidKey); err != nil {
		t.Fatalf("second disallow: %v", err)
	}
	ok, err := store.Allowed(ctx, pub)
	if err != nil || ok {
		t.Fatalf("still allowed: %v %v", ok, err)
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

func TestMigrateAllowed(t *testing.T) {
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
	if _, err := db.Exec(`
		CREATE TABLE allowed (
			kind TEXT NOT NULL, id TEXT NOT NULL, reason TEXT NOT NULL, created_at INTEGER NOT NULL,
			PRIMARY KEY (kind, id)
		)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO allowed (kind, id, reason, created_at) VALUES ('nostr', ?, 'paid', 1)`, paidKey); err != nil {
		t.Fatal(err)
	}
	store, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := store.Allowed(context.Background(), Publisher{Kind: KindNostr, ID: paidKey})
	if err != nil || !ok {
		t.Fatalf("migrated paid access: %v %v", ok, err)
	}
	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'allowed'`).Scan(&name)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old table still present: %v %q", err, name)
	}
}
