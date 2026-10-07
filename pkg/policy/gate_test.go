package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
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
	if wot.calls != 2 {
		t.Fatalf("web of trust calls = %d, want 2", wot.calls)
	}
	var n int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admissions WHERE "check" = ?`, CheckPaid).Scan(&n); err != nil || n != 1 {
		t.Fatalf("paid admissions = %d, %v", n, err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admissions WHERE "check" = ? AND score = 0.9`, CheckWebOfTrust).Scan(&n); err != nil || n != 1 {
		t.Fatalf("web of trust admissions = %d, %v", n, err)
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
