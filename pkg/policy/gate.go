package policy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ErrNotAdmitted is returned when a publisher is neither the catalog key, a
// paid publisher, nor above the web of trust threshold.
var ErrNotAdmitted = errors.New("publisher did not pass the gate")

// Check names which gate a publish or upload passed. The admissions table
// records it.
const (
	CheckWebOfTrust = "web of trust"
	CheckPaid       = "paid"
)

// WebOfTrust answers whether a pubkey is above the web of trust threshold,
// and with what score. Vertex implements it.
type WebOfTrust interface {
	Allow(ctx context.Context, pubkey string) (bool, float64, error)
}

// Gate decides whether a publisher who is not the catalog key may publish a
// listing or upload a file. It runs after the blacklist. A nil WebOfTrust,
// or a web of trust check that cannot be reached, leaves only the paid list.
type Gate struct {
	store      *Store
	catalogKey string
	wot        WebOfTrust
}

// NewGate builds the gate for one catalog key. wot may be nil.
func NewGate(store *Store, catalogKey string, wot WebOfTrust) *Gate {
	return &Gate{store: store, catalogKey: strings.ToLower(strings.TrimSpace(catalogKey)), wot: wot}
}

// Admit returns nil when pubkey may publish and ErrNotAdmitted when it may
// not. A web of trust outage is a rejection, not a failed check. Every
// developer admission is recorded with the check that passed.
func (g *Gate) Admit(ctx context.Context, pubkey string) error {
	if g == nil {
		return nil
	}
	pub, err := Normalize(KindNostr, pubkey)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotAdmitted, err)
	}
	if pub.ID == g.catalogKey {
		return nil
	}
	paid, err := g.store.Allowed(ctx, pub)
	if err != nil {
		return fmt.Errorf("allowed list: %w", err)
	}
	if paid {
		g.record(ctx, pub.ID, CheckPaid, 0)
		return nil
	}
	if g.wot == nil {
		return ErrNotAdmitted
	}
	ok, score, err := g.wot.Allow(ctx, pub.ID)
	if err != nil {
		slog.Error("policy: web of trust unavailable", "err", err, "pubkey", pub.ID)
		return ErrNotAdmitted
	}
	if !ok {
		return ErrNotAdmitted
	}
	g.record(ctx, pub.ID, CheckWebOfTrust, score)
	return nil
}

func (g *Gate) record(ctx context.Context, pubkey, check string, score float64) {
	if err := g.store.RecordAdmission(ctx, pubkey, check, score); err != nil {
		slog.Error("policy: failed to record admission", "err", err, "pubkey", pubkey, "check", check)
	}
}

// Allowed reports whether this publisher is on the paid publisher list.
func (s *Store) Allowed(ctx context.Context, pub Publisher) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM allowed WHERE kind = ? AND id = ?`, pub.Kind, pub.ID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// Allow puts a publisher on the paid publisher list. It does not touch the
// blacklist.
func (s *Store) Allow(ctx context.Context, pub Publisher, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ErrReason
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO allowed (kind, id, reason, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(kind, id) DO UPDATE SET reason = excluded.reason`,
		pub.Kind, pub.ID, reason, time.Now().Unix())
	return err
}

// Disallow removes a publisher from the paid publisher list. A missing row is
// success.
func (s *Store) Disallow(ctx context.Context, kind, id string) error {
	pub, err := Normalize(kind, id)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM allowed WHERE kind = ? AND id = ?`, pub.Kind, pub.ID)
	return err
}

// AllowedRow is one paid publisher.
type AllowedRow struct {
	Kind      string
	ID        string
	Reason    string
	CreatedAt int64
}

// AllowedList returns the paid publisher list, newest first.
func (s *Store) AllowedList(ctx context.Context) ([]AllowedRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, id, reason, created_at FROM allowed ORDER BY created_at DESC, kind, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AllowedRow
	for rows.Next() {
		var row AllowedRow
		if err := rows.Scan(&row.Kind, &row.ID, &row.Reason, &row.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// RecordAdmission stores which check a developer publish or upload passed.
func (s *Store) RecordAdmission(ctx context.Context, pubkey, check string, score float64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO admissions (pubkey, "check", score, created_at) VALUES (?, ?, ?, ?)`,
		pubkey, check, score, time.Now().Unix())
	return err
}
