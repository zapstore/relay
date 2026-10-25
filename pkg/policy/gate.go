package policy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

// ErrNotAdmitted is returned when a publisher is not the catalog key, has not
// published a kind 32267, is not on the whitelist, is not above the web of
// trust threshold, and is not publishing an app the relay already lists.
var ErrNotAdmitted = errors.New("publisher did not pass the gate")

// WebOfTrust answers whether a pubkey is above the web of trust threshold,
// and with what score. Vertex implements it.
type WebOfTrust interface {
	Allow(ctx context.Context, pubkey string) (bool, float64, error)
}

// Gate decides whether a publisher who is not the catalog key may publish a
// listing or upload a file. It runs after the blacklist. A nil WebOfTrust,
// or a web of trust check that cannot be reached, leaves the catalog key,
// a key that already published a kind 32267, a whitelisted key, and a listing
// for an app id that already has a kind 32267.
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
// not. An existing kind 32267 and the whitelist are checked before Vertex.
// A web of trust outage is a rejection, not a failed check.
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
	listed, err := g.store.Allowed(ctx, pub.ID)
	if err != nil {
		return fmt.Errorf("whitelist: %w", err)
	}
	if listed {
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

// AdmitListing is Admit for one listing event. An app id that already has a
// kind 32267 skips web of trust and the whitelist. The developer slot is
// decided later, from the C1 proof. An empty appID is a new or untagged
// listing and still goes through Admit.
func (g *Gate) AdmitListing(ctx context.Context, pubkey, appID string) error {
	if g == nil {
		return nil
	}
	listed, err := g.store.appListed(ctx, appID)
	if err != nil {
		return fmt.Errorf("listed app: %w", err)
	}
	if listed {
		return nil
	}
	return g.Admit(ctx, pubkey)
}

// AdmitUpload is Admit for a Blossom upload. The upload names no app id, so
// a key that does not pass Admit is still accepted when it is the active C1
// owner or delegate of a certificate on an app the relay already lists.
// zsp uploads the APK before the listing events.
func (g *Gate) AdmitUpload(ctx context.Context, pubkey string) error {
	if g == nil {
		return nil
	}
	err := g.Admit(ctx, pubkey)
	if !errors.Is(err, ErrNotAdmitted) {
		return err
	}
	pub, normErr := Normalize(KindNostr, pubkey)
	if normErr != nil {
		return fmt.Errorf("%w: %v", ErrNotAdmitted, normErr)
	}
	holds, holdErr := g.store.proofForListedApp(ctx, pub.ID)
	if holdErr != nil {
		return fmt.Errorf("listed app proof: %w", holdErr)
	}
	if holds {
		return nil
	}
	return ErrNotAdmitted
}

// appListed reports whether a kind 32267 for appID is already stored.
func (s *Store) appListed(ctx context.Context, appID string) (bool, error) {
	if appID == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM events e
		JOIN tags t ON t.event_id = e.id AND t.key = 'd' AND t.value = ?
		WHERE e.kind = ?
		LIMIT 1
	`, appID, events.KindApp).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// proofForListedApp reports whether pubkey is the active C1 owner or delegate
// of a certificate named on an asset of an app that already has a kind 32267.
func (s *Store) proofForListedApp(ctx context.Context, pubkey string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.pubkey, e.created_at, e.id, e.tags
		FROM events e
		WHERE e.kind = ?
		  AND (
			e.pubkey = ?
			OR EXISTS (
				SELECT 1 FROM json_each(e.tags)
				WHERE json_extract(value, '$[0]') = 'delegation'
				  AND lower(json_extract(value, '$[1]')) = ?
			)
		  )
	`, events.KindIdentityProof, pubkey, pubkey)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	current := map[string]nostr.Event{}
	for rows.Next() {
		var event nostr.Event
		var raw string
		var created int64
		if err := rows.Scan(&event.PubKey, &created, &event.ID, &raw); err != nil {
			return false, err
		}
		event.Kind = events.KindIdentityProof
		event.CreatedAt = nostr.Timestamp(created)
		if err := event.Tags.Scan(raw); err != nil {
			return false, fmt.Errorf("proof tags: %w", err)
		}
		cert, ok := events.Find(event.Tags, "d")
		if !ok || cert == "" {
			continue
		}
		key := event.PubKey + "\x00" + strings.ToLower(cert)
		if prev, exists := current[key]; exists && !newerProof(event, prev) {
			continue
		}
		current[key] = event
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	now := time.Now()
	for _, event := range current {
		if !events.ProofActive(event, now) {
			continue
		}
		delegate, _ := events.Find(event.Tags, "delegation")
		if event.PubKey != pubkey && !strings.EqualFold(delegate, pubkey) {
			continue
		}
		cert, _ := events.Find(event.Tags, "d")
		listed, err := s.certOnListedApp(ctx, cert)
		if err != nil {
			return false, err
		}
		if listed {
			return true, nil
		}
	}
	return false, nil
}

func newerProof(next, current nostr.Event) bool {
	if next.CreatedAt != current.CreatedAt {
		return next.CreatedAt > current.CreatedAt
	}
	return next.ID < current.ID
}

// certOnListedApp reports whether a stored asset names cert and its app id
// already has a kind 32267.
func (s *Store) certOnListedApp(ctx context.Context, cert string) (bool, error) {
	cert = strings.ToLower(strings.TrimSpace(cert))
	if cert == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1
		FROM tags tcert
		JOIN events asset ON asset.id = tcert.event_id AND asset.kind = ?
		JOIN tags ti ON ti.event_id = asset.id AND ti.key = 'i'
		JOIN tags td ON td.key = 'd' AND td.value = ti.value
		JOIN events app ON app.id = td.event_id AND app.kind = ?
		WHERE tcert.key = 'apk_certificate_hash' AND lower(tcert.value) = ?
		LIMIT 1
	`, events.KindAsset, events.KindApp, cert).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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
