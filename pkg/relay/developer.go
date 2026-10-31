package relay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/rely/v2"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/relay/store"
)

var errCertificate = errors.New("asset certificate does not match this app")

// ListingCertificate rejects an asset that names none of the app's certificate.
// Whether a pubkey may publish is decided at promotion, once the APK has been
// read: a new developer's proof arrives after its listing.
func ListingCertificate(db store.T, catalogKey string) func(rely.Client, *nostr.Event) error {
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
		pinned, ok, err := appCertificate(ctx, db, catalogKey, appID)
		if err != nil || !ok {
			return err
		}
		if !assetNames(*event, pinned) {
			return errCertificate
		}
		return nil
	}
}

// isActiveDeveloper reports whether pubkey is the active C1 owner or delegate
// for the app's certificate.
func isActiveDeveloper(ctx context.Context, db store.T, catalogKey, appID, pubkey string) (bool, error) {
	cert, ok, err := appCertificate(ctx, db, catalogKey, appID)
	if err != nil || !ok {
		return false, err
	}
	owner, delegate, ok, err := activeDeveloper(ctx, db, []string{cert}, nil)
	if err != nil || !ok {
		return false, err
	}
	return pubkey == owner || (delegate != "" && pubkey == delegate), nil
}

// appCertificate is the current signer: the first apk_certificate_hash on
// the current promoted asset for appID. It is not the lineage root. The
// catalog key's asset wins over a developer asset. Otherwise the newest
// asset wins. There is no separate pin row.
func appCertificate(ctx context.Context, db store.T, catalogKey, appID string) (string, bool, error) {
	var hash string
	err := db.DB.QueryRowContext(ctx, `
		SELECT tcert.value
		FROM events e
		JOIN tags tapp ON tapp.event_id = e.id AND tapp.key = 'i' AND tapp.value = ?
		JOIN tags tcert ON tcert.event_id = e.id AND tcert.key = 'apk_certificate_hash'
		WHERE e.kind = ?
		ORDER BY CASE WHEN e.pubkey = ? THEN 0 ELSE 1 END, e.created_at DESC, e.id DESC, tcert.rowid
		LIMIT 1
	`, appID, events.KindAsset, catalogKey).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		slog.Error("listing: read certificate", "err", err, "app_id", appID)
		return "", false, ErrInternal
	}
	return strings.ToLower(hash), true, nil
}

// activeDeveloper chooses the C1 owner for the certificates in certs. Callers
// pass the app's current signer and nothing else. A proof for an ancestor
// does not count: that key may have been retired because it leaked.
func activeDeveloper(ctx context.Context, db store.T, certs []string, extra []nostr.Event) (owner, delegate string, ok bool, err error) {
	if len(certs) == 0 {
		return "", "", false, nil
	}
	args := make([]any, 0, 1+len(certs))
	args = append(args, events.KindIdentityProof)
	marks := make([]string, len(certs))
	for i, cert := range certs {
		marks[i] = "?"
		args = append(args, strings.ToLower(cert))
	}
	rows, err := db.DB.QueryContext(ctx, `
		SELECT e.pubkey, e.created_at, e.id, e.tags
		FROM events e
		JOIN tags t ON t.event_id = e.id
		WHERE e.kind = ? AND t.key = 'd' AND lower(t.value) IN (`+strings.Join(marks, ",")+`)
	`, args...)
	if err != nil {
		slog.Error("listing: read proofs", "err", err)
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

// activeDevelopers is the active C1 owner and delegate for the app's
// certificate. Both are empty when the app has no certificate or no active
// proof.
func (r *T) activeDevelopers(ctx context.Context, appID string) (owner, delegate string, err error) {
	cert, ok, err := appCertificate(ctx, r.store, r.config.Info.Pubkey, appID)
	if err != nil || !ok {
		return "", "", err
	}
	owner, delegate, _, err = activeDeveloper(ctx, r.store, []string{cert}, nil)
	return owner, delegate, err
}

// storedRelease is a stored kind 30063 with the version_code of its assets.
type storedRelease struct {
	id, pubkey, d, channel string
	code                   int64
	at                     int64 // created_at
	assets                 []string
}

// storedReleaseCodes reads every stored release of appID, whoever signed it,
// with the greatest version_code among the assets it names.
func (r *T) storedReleaseCodes(ctx context.Context, appID string) ([]storedRelease, error) {
	rows, err := r.store.DB.QueryContext(ctx, `
		SELECT r.id, r.pubkey, r.created_at, r.tags, a.id, a.tags FROM events r
		JOIN tags ti ON ti.event_id = r.id AND ti.key = 'i' AND ti.value = ?
		JOIN tags te ON te.event_id = r.id AND te.key = 'e'
		JOIN events a ON a.id = te.value AND a.kind = ?
		WHERE r.kind = ?
	`, appID, events.KindAsset, events.KindRelease)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedRelease
	index := map[string]int{}
	for rows.Next() {
		var id, pub, rawRelease, assetID, rawAsset string
		var at64 int64
		if err := rows.Scan(&id, &pub, &at64, &rawRelease, &assetID, &rawAsset); err != nil {
			return nil, err
		}
		at, seen := index[id]
		if !seen {
			var tags nostr.Tags
			if err := tags.Scan(rawRelease); err != nil {
				continue
			}
			channel, _ := events.Find(tags, "c")
			d, _ := events.Find(tags, "d")
			index[id] = len(out)
			at = len(out)
			out = append(out, storedRelease{id: id, pubkey: pub, d: d, channel: channel, at: at64})
		}
		var assetTags nostr.Tags
		if err := assetTags.Scan(rawAsset); err != nil {
			continue
		}
		out[at].code = max(out[at].code, maxVersionCode([]nostr.Event{{Tags: assetTags}}))
		out[at].assets = append(out[at].assets, assetID)
	}
	return out, rows.Err()
}

// supersede keeps the app's current listing whole once release is promoted.
// It does nothing unless release is the current one: no release of the
// catalog key, the owner, or the delegate has a greater version_code.
// Otherwise:
//   - keepApp, when set, is the signer of the kind 32267 the release belongs
//     to, and every other signer's kind 32267 for the app is deleted;
//   - the releases of any other pubkey whose version_code is not lower, and
//     the assets they name, are deleted: a former owner's key may have leaked,
//     and its release must not outrank the current one.
//
// Older releases stay. It is idempotent.
func (r *T) supersede(ctx context.Context, release *nostr.Event, keepApp string) error {
	if release == nil {
		return nil
	}
	appID, ok := listingAppID(release)
	if !ok {
		return nil
	}
	owner, delegate, err := r.activeDevelopers(ctx, appID)
	if err != nil {
		return err
	}
	// The signer was authorized when this release was promoted. A rotation can
	// move the app's certificate past the proof it used.
	authorized := []string{r.config.Info.Pubkey, owner, delegate, release.PubKey}
	stored, err := r.storedReleaseCodes(ctx, appID)
	if err != nil {
		return err
	}
	channel, _ := events.Find(release.Tags, "c")
	var code, floor int64
	for _, other := range stored {
		if other.id == release.ID {
			code = other.code
		}
		if other.channel == channel && slices.Contains(authorized, other.pubkey) {
			floor = max(floor, other.code)
		}
	}
	if code < floor {
		return nil
	}
	var ids []string
	if keepApp != "" {
		apps, err := appIDsOf(ctx, r.store.DB, `
			SELECT e.id FROM events e JOIN tags t ON t.event_id = e.id AND t.key = 'd' AND t.value = ?
			WHERE e.kind = ? AND e.pubkey != ?`, appID, events.KindApp, keepApp)
		if err != nil {
			return err
		}
		ids = append(ids, apps...)
	}
	for _, other := range stored {
		if other.channel == channel && !slices.Contains(authorized, other.pubkey) && other.code >= code {
			ids = append(ids, other.id)
			ids = append(ids, other.assets...)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := r.store.Delete(ctx, nostr.Filter{IDs: ids}); err != nil {
		return fmt.Errorf("supersede listing of %s: %w", appID, err)
	}
	slog.Info("listing: superseded", "app_id", appID, "release", release.ID, "events", len(ids))
	return nil
}

func listingAppID(event *nostr.Event) (string, bool) {
	key := "i"
	if event.Kind == events.KindApp {
		key = "d"
	}
	id, ok := events.Find(event.Tags, key)
	return id, ok && id != ""
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
	holds, err := isActiveDeveloper(ctx, r.store, r.config.Info.Pubkey, appID, event.PubKey)
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
	pending, err := r.dropPendingApp(ctx, appID)
	if err != nil {
		return n, err
	}
	r.SyncSearch(ctx, appID)
	return n + pending, nil
}
