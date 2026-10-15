package policy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/zapstore/relay/pkg/events"
)

// ErrNotAdmitted is returned when a publisher is not the catalog key, has not
// published a kind 32267, and is not above the web of trust threshold.
var ErrNotAdmitted = errors.New("publisher did not pass the gate")

// WebOfTrust answers whether a pubkey is above the web of trust threshold,
// and with what score. Vertex implements it.
type WebOfTrust interface {
	Allow(ctx context.Context, pubkey string) (bool, float64, error)
}

// Gate decides whether a publisher who is not the catalog key may publish a
// listing or upload a file. It runs after the blacklist. A nil WebOfTrust,
// or a web of trust check that cannot be reached, leaves only a key that
// already published a kind 32267.
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
// not. An existing kind 32267 is checked before Vertex. A web of trust
// outage is a rejection, not a failed check.
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
	published, err := g.store.PublishedApp(ctx, pub.ID)
	if err != nil {
		return fmt.Errorf("published app: %w", err)
	}
	if published {
		return nil
	}
	if g.wot == nil {
		return ErrNotAdmitted
	}
	ok, _, err := g.wot.Allow(ctx, pub.ID)
	if err != nil {
		slog.Error("policy: web of trust unavailable", "err", err, "pubkey", pub.ID)
		return ErrNotAdmitted
	}
	if !ok {
		return ErrNotAdmitted
	}
	return nil
}

// PublishedApp reports whether pubkey has a kind 32267 in the relay.
func (s *Store) PublishedApp(ctx context.Context, pubkey string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM events WHERE pubkey = ? AND kind = ? LIMIT 1
	`, pubkey, events.KindApp).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
