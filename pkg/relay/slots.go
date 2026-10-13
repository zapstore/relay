package relay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/rely/v2"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/relay/store"
)

var errCertificate = errors.New("asset certificate does not match this app")

// ListingSlots rejects an asset that names none of the app's certificate.
// Which pubkey holds a slot is decided at promotion, once the APK has been
// read: a new developer's proof arrives after its listing.
func ListingSlots(db store.T, catalogKey string) func(rely.Client, *nostr.Event) error {
	return func(_ rely.Client, event *nostr.Event) error {
		if event.Kind != events.KindAsset || event.PubKey == catalogKey {
			return nil
		}
		appID, ok := events.Find(event.Tags, "i")
		if !ok || appID == "" {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		pinned, ok, err := appCertificate(ctx, db, appID)
		if err != nil || !ok {
			return err
		}
		if !slices.Contains(assetCerts(*event), pinned) {
			return errCertificate
		}
		return nil
	}
}

// holdsDeveloperSlot reports whether pubkey is the active C1 owner or delegate
// for the app's certificate.
func holdsDeveloperSlot(ctx context.Context, db store.T, appID, pubkey string) (bool, error) {
	cert, ok, err := appCertificate(ctx, db, appID)
	if err != nil || !ok {
		return false, err
	}
	owner, delegate, ok, err := activeDeveloper(ctx, db, cert, nil)
	if err != nil || !ok {
		return false, err
	}
	return pubkey == owner || (delegate != "" && pubkey == delegate), nil
}

func appCertificate(ctx context.Context, db store.T, appID string) (string, bool, error) {
	var hash string
	err := db.DB.QueryRowContext(ctx, `SELECT hash FROM app_certificate WHERE app_id = ?`, appID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		slog.Error("listing slot: read certificate", "err", err, "app_id", appID)
		return "", false, ErrInternal
	}
	return hash, true, nil
}

func setAppCertificate(ctx context.Context, db store.T, appID, cert string, overwrite bool) error {
	query := `INSERT INTO app_certificate (app_id, hash) VALUES (?, ?) ON CONFLICT(app_id) DO NOTHING`
	if overwrite {
		query = `INSERT INTO app_certificate (app_id, hash) VALUES (?, ?)
			ON CONFLICT(app_id) DO UPDATE SET hash = excluded.hash`
	}
	if _, err := db.DB.ExecContext(ctx, query, appID, cert); err != nil {
		slog.Error("listing slot: pin certificate", "err", err, "app_id", appID)
		return ErrInternal
	}
	return nil
}

// activeDeveloper chooses the C1 owner for cert from the stored proofs and
// extra, pending proofs already checked against an APK.
func activeDeveloper(ctx context.Context, db store.T, cert string, extra []nostr.Event) (owner, delegate string, ok bool, err error) {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT e.pubkey, e.created_at, e.id, e.tags
		FROM events e
		JOIN tags t ON t.event_id = e.id
		WHERE e.kind = ? AND t.key = 'd' AND t.value = ?
	`, events.KindIdentityProof, cert)
	if err != nil {
		slog.Error("listing slot: read proofs", "err", err)
		return "", "", false, ErrInternal
	}
	defer rows.Close()
	current := map[string]nostr.Event{}
	for rows.Next() {
		var event nostr.Event
		var raw string
		var created int64
		if err := rows.Scan(&event.PubKey, &created, &event.ID, &raw); err != nil {
			return "", "", false, ErrInternal
		}
		event.Kind = events.KindIdentityProof
		event.CreatedAt = nostr.Timestamp(created)
		if err := event.Tags.Scan(raw); err != nil {
			return "", "", false, fmt.Errorf("proof tags: %w", err)
		}
		if prev, exists := current[event.PubKey]; exists && !newerProof(event, prev) {
			continue
		}
		current[event.PubKey] = event
	}
	if err := rows.Err(); err != nil {
		return "", "", false, ErrInternal
	}
	for _, event := range extra {
		if prev, exists := current[event.PubKey]; exists && !newerProof(event, prev) {
			continue
		}
		current[event.PubKey] = event
	}
	now := time.Now()
	var best *nostr.Event
	for _, event := range current {
		if !events.ProofActive(event, now) {
			continue
		}
		if best == nil || newerProof(event, *best) {
			copy := event
			best = &copy
		}
	}
	if best == nil {
		return "", "", false, nil
	}
	delegate, _ = events.Find(best.Tags, "delegation")
	return best.PubKey, delegate, true, nil
}

func newerProof(next, current nostr.Event) bool {
	if next.CreatedAt != current.CreatedAt {
		return next.CreatedAt > current.CreatedAt
	}
	return next.ID < current.ID
}

func (r *T) replaceOtherDeveloper(ctx context.Context, event *nostr.Event) {
	if event.PubKey == r.config.Info.Pubkey {
		return
	}
	appID, ok := listingAppID(event)
	if !ok {
		return
	}
	complete, err := developerListingComplete(ctx, r.store, event.PubKey, appID)
	if err != nil || !complete {
		return
	}
	ids, err := otherDeveloperEvents(ctx, r.store, r.config.Info.Pubkey, event.PubKey, appID)
	if err != nil || len(ids) == 0 {
		return
	}
	if _, err := r.store.Delete(ctx, nostr.Filter{IDs: ids}); err != nil {
		slog.Error("listing slot: replace developer", "err", err, "app_id", appID)
		return
	}
	slog.Info("listing slot: replaced developer listing", "app_id", appID, "pubkey", event.PubKey)
}

func listingAppID(event *nostr.Event) (string, bool) {
	key := "i"
	if event.Kind == events.KindApp {
		key = "d"
	}
	id, ok := events.Find(event.Tags, key)
	return id, ok && id != ""
}

func developerListingComplete(ctx context.Context, db store.T, pubkey, appID string) (bool, error) {
	var one int
	err := db.DB.QueryRowContext(ctx, `
		SELECT 1 FROM events e
		JOIN tags t ON t.event_id = e.id
		WHERE e.kind = ? AND e.pubkey = ? AND t.key = 'd' AND t.value = ?
		LIMIT 1
	`, events.KindApp, pubkey, appID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	rows, err := db.DB.QueryContext(ctx, `
		SELECT e.tags FROM events e
		JOIN tags t ON t.event_id = e.id
		WHERE e.kind = ? AND e.pubkey = ? AND t.key = 'i' AND t.value = ?
	`, events.KindRelease, pubkey, appID)
	if err != nil {
		return false, err
	}
	var raws []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return false, err
		}
		raws = append(raws, raw)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	for _, raw := range raws {
		var tags nostr.Tags
		if err := tags.Scan(raw); err != nil {
			continue
		}
		release, err := events.ParseRelease(&nostr.Event{Kind: events.KindRelease, Tags: tags})
		if err != nil || len(release.AssetIDs) == 0 {
			continue
		}
		ready := true
		for _, id := range release.AssetIDs {
			var n int
			err := db.DB.QueryRowContext(ctx, `SELECT 1 FROM events WHERE id = ? AND kind = ? AND pubkey = ?`, id, events.KindAsset, pubkey).Scan(&n)
			if err != nil {
				ready = false
				break
			}
		}
		if ready {
			return true, nil
		}
	}
	return false, nil
}

func otherDeveloperEvents(ctx context.Context, db store.T, catalogKey, pubkey, appID string) ([]string, error) {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT e.pubkey FROM events e
		JOIN tags t ON t.event_id = e.id
		WHERE e.kind = ? AND t.key = 'd' AND t.value = ?
	`, events.KindApp, appID)
	if err != nil {
		return nil, err
	}
	var pubs []string
	for rows.Next() {
		var pub string
		if err := rows.Scan(&pub); err != nil {
			rows.Close()
			return nil, err
		}
		if pub == catalogKey || pub == pubkey {
			continue
		}
		pubs = append(pubs, pub)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var ids []string
	for _, pub := range pubs {
		found, err := eventsByApp(ctx, db, pub, appID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, found...)
	}
	return ids, nil
}

func eventsByApp(ctx context.Context, db store.T, pubkey, appID string) ([]string, error) {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT e.id FROM events e
		JOIN tags t ON t.event_id = e.id
		WHERE e.pubkey = ? AND (
			(e.kind = ? AND t.key = 'd' AND t.value = ?)
			OR (e.kind IN (?, ?) AND t.key = 'i' AND t.value = ?)
		)
	`, pubkey, events.KindApp, appID, events.KindRelease, events.KindAsset, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *T) isDeveloperRemoval(ctx context.Context, event *nostr.Event) bool {
	appID := developerRemovalApp(event)
	if appID == "" {
		return false
	}
	var one int
	err := r.store.DB.QueryRowContext(ctx, `
		SELECT 1 FROM events e
		JOIN tags t ON t.event_id = e.id
		WHERE e.kind = ? AND e.pubkey = ? AND t.key = 'd' AND t.value = ?
		LIMIT 1
	`, events.KindApp, event.PubKey, appID).Scan(&one)
	if err != nil {
		return false
	}
	holds, err := holdsDeveloperSlot(ctx, r.store, appID, event.PubKey)
	return err == nil && holds
}

func developerRemovalApp(event *nostr.Event) string {
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != "a" {
			continue
		}
		ref, err := events.ParseAppIdentifier(tag[1])
		if err != nil || ref.Pubkey != event.PubKey || ref.AppID == "" {
			continue
		}
		return ref.AppID
	}
	return ""
}

func (r *T) deleteAppListings(ctx context.Context, appID string) (int, error) {
	rows, err := r.store.DB.QueryContext(ctx, `
		SELECT DISTINCT pubkey FROM events e
		JOIN tags t ON t.event_id = e.id
		WHERE (e.kind = ? AND t.key = 'd' AND t.value = ?)
		   OR (e.kind IN (?, ?) AND t.key = 'i' AND t.value = ?)
	`, events.KindApp, appID, events.KindRelease, events.KindAsset, appID)
	if err != nil {
		return 0, fmt.Errorf("list app %s: %w", appID, err)
	}
	var pubs []string
	for rows.Next() {
		var pub string
		if err := rows.Scan(&pub); err != nil {
			rows.Close()
			return 0, err
		}
		pubs = append(pubs, pub)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	var ids []string
	for _, pub := range pubs {
		found, err := eventsByApp(ctx, r.store, pub, appID)
		if err != nil {
			return 0, err
		}
		ids = append(ids, found...)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	n, err := r.store.Delete(ctx, nostr.Filter{IDs: ids})
	if err != nil {
		return 0, fmt.Errorf("delete app %s: %w", appID, err)
	}
	if _, err := r.store.DB.ExecContext(ctx, `DELETE FROM app_certificate WHERE app_id = ?`, appID); err != nil {
		return n, fmt.Errorf("clear certificate %s: %w", appID, err)
	}
	pending, err := r.dropPendingApp(ctx, appID)
	if err != nil {
		return n, err
	}
	return n + pending, nil
}
