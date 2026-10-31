package relay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/blossom"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/relay/store"
)

// A listing kept in the buffer zone is answered OK with one of these. Clients
// tell it from a stored event by the "pending:" prefix.
const (
	msgBlobPending    = "pending: the event will be saved when the referenced blob is uploaded"
	msgListingPending = "pending: the listing will be saved once its checks pass"
)

var fileHash = regexp.MustCompile(`[0-9a-f]{64}`)

func listingKind(kind int) bool {
	switch kind {
	case events.KindApp, events.KindRelease, events.KindAsset:
		return true
	default:
		return false
	}
}

func listingKinds() []int {
	return []int{events.KindApp, events.KindRelease, events.KindAsset}
}

// bufferedKinds are the kinds kept in the buffer zone.
func bufferedKinds() []int {
	return append(listingKinds(), events.KindIdentityProof)
}

// holdListing keeps a listing in the buffer zone until promotion. A
// developer listing waits for its APK read and the C1 check. A catalog-key
// listing waits only for the event checks, which take milliseconds: the
// indexer already verified its APK, so the relay does not read it, and an
// event that fails a check is dropped and never stored. It does not open the
// APK: the reader does, off the event path.
func (r *T) holdListing(ctx context.Context, event *nostr.Event) persistResult {
	if event.PubKey == r.config.Info.Pubkey && event.Kind == events.KindAsset && assetSigner(*event) == "" {
		return persistResult{msg: "the asset names no apk_certificate_hash"}
	}
	if _, err := r.store.SavePending(ctx, event); err != nil {
		slog.Error("relay: failed to save pending listing", "event", event.ID, "error", err)
		return persistResult{msg: ErrInternal.Error()}
	}
	if err := supersedePending(ctx, r.store, event); err != nil {
		slog.Error("relay: failed to supersede pending listing", "event", event.ID, "error", err)
		return persistResult{msg: ErrInternal.Error()}
	}

	appID, ok := listingAppID(event)
	if !ok {
		return persistResult{ok: true}
	}
	refused, err := r.promoteListing(ctx, event.PubKey, appID, event.ID)
	if err != nil {
		slog.Error("relay: failed to promote listing", "event", event.ID, "error", err)
		return persistResult{msg: ErrInternal.Error()}
	}

	stored, err := r.store.Has(ctx, nostr.Filter{IDs: []string{event.ID}})
	if err != nil {
		slog.Error("relay: failed to check promoted listing", "event", event.ID, "error", err)
		return persistResult{msg: ErrInternal.Error()}
	}
	if stored {
		return persistResult{ok: true, broadcast: true}
	}
	kept, err := pendingHas(ctx, r.store, event.ID)
	if err != nil {
		slog.Error("relay: failed to check pending listing", "event", event.ID, "error", err)
		return persistResult{msg: ErrInternal.Error()}
	}
	if !kept {
		if reason, ok := refused[event.ID]; ok {
			return persistResult{msg: reason}
		}
		return persistResult{ok: true}
	}
	if event.Kind == events.KindAsset && r.assetURL(event.Tags) == "" {
		ready, err := r.assetInBlossom(ctx, event)
		if err != nil {
			slog.Error("relay: failed to check asset blob", "event", event.ID, "error", err)
			return persistResult{msg: ErrInternal.Error()}
		}
		if !ready {
			return persistResult{ok: true, msg: msgBlobPending}
		}
	}
	return persistResult{ok: true, msg: msgListingPending}
}

// msgNamesAppEvent starts every refusal of a catalog release over the app
// event it names. The indexer matches it and must stay in step:
// indexer/pkg/publisher.namesAppEventPrefix.
const msgNamesAppEvent = "the release names an app event"

// developerApp returns the stored kind 32267 of appID that the active owner
// or delegate signed, or nil.
func (r *T) developerApp(ctx context.Context, appID string) (*nostr.Event, error) {
	apps, err := loadEvents(ctx, r.store.DB, `
		SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM events e JOIN tags t ON t.event_id = e.id AND t.key = 'd' AND t.value = ?
		WHERE e.kind = ? AND e.pubkey != ?
		ORDER BY e.created_at DESC, e.id ASC`, appID, events.KindApp, r.config.Info.Pubkey)
	if err != nil {
		return nil, err
	}
	for i := range apps {
		holds, err := isActiveDeveloper(ctx, r.store, r.config.Info.Pubkey, appID, apps[i].PubKey)
		if err != nil {
			return nil, err
		}
		if holds {
			return &apps[i], nil
		}
	}
	return nil, nil
}

// catalogReleaseApp is the kind 32267 a catalog-key release belongs to: the
// catalog key's own, or, when the release names another signer's in its `a`
// tag, that one. The named signer must be the active owner or delegate, or
// the release is refused: refusal is its reason. A nil app with no refusal
// means the app event has not arrived.
func (r *T) catalogReleaseApp(ctx context.Context, own *nostr.Event, release nostr.Event, appID string) (app *nostr.Event, refusal string, err error) {
	got, ok := events.Find(release.Tags, "a")
	if !ok {
		return own, "", nil
	}
	ref, parseErr := events.ParseAppIdentifier(got)
	if parseErr != nil || ref.AppID != appID || ref.Pubkey == r.config.Info.Pubkey {
		return own, "", nil
	}
	named, err := r.listingApp(ctx, ref.Pubkey, appID, nil)
	if err != nil {
		return nil, "", err
	}
	if named == nil {
		return nil, msgNamesAppEvent + " the relay does not have", nil
	}
	holds, err := isActiveDeveloper(ctx, r.store, r.config.Info.Pubkey, appID, ref.Pubkey)
	if err != nil {
		return nil, "", err
	}
	if !holds {
		return nil, msgNamesAppEvent + " whose signer is not the app's developer", nil
	}
	return named, "", nil
}

// dropCatalogRelease removes a refused catalog release and the assets it
// names. The app goes with them when no release of that app is left.
func (r *T) dropCatalogRelease(ctx context.Context, app *nostr.Event, release nostr.Event, assets map[string]nostr.Event, reason string, refused map[string]string) error {
	slog.Warn("listing refused", "release", release.ID, "pubkey", release.PubKey, "reason", reason)
	ids := []string{release.ID}
	for _, id := range events.FindAll(release.Tags, "e") {
		if _, ok := assets[id]; ok {
			ids = append(ids, id)
		}
	}
	if _, err := r.store.Delete(ctx, nostr.Filter{IDs: ids}); err != nil {
		return err
	}
	for _, id := range ids {
		refused[id] = reason
	}
	// Any signer's release keeps the app event: a catalog app event arrives
	// before its release, and an older release judged now is not its own.
	left, err := r.storedReleaseCodes(ctx, listingAppIDOrEmpty(&release))
	if err != nil {
		return err
	}
	if len(left) == 0 && app != nil && app.PubKey == release.PubKey {
		if _, err := r.store.Delete(ctx, nostr.Filter{IDs: []string{app.ID}}); err != nil {
			return err
		}
		refused[app.ID] = reason
	}
	return nil
}

// supersedePending keeps the newest pending app, release, or proof per pubkey
// and d tag.
func supersedePending(ctx context.Context, db store.T, event *nostr.Event) error {
	switch event.Kind {
	case events.KindApp, events.KindRelease, events.KindIdentityProof:
	default:
		return nil
	}
	d, ok := events.Find(event.Tags, "d")
	if !ok || d == "" {
		return nil
	}
	var pending []nostr.Event
	var err error
	if appID, ok := listingAppID(event); ok && event.Kind != events.KindIdentityProof {
		pending, err = db.PendingListing(ctx, appID)
	} else {
		pending, err = db.QueryPending(ctx, event.Kind)
	}
	if err != nil {
		return err
	}
	var newer bool
	var drop []string
	for _, other := range pending {
		if other.ID == event.ID || other.Kind != event.Kind || other.PubKey != event.PubKey {
			continue
		}
		otherD, ok := events.Find(other.Tags, "d")
		if !ok || otherD != d {
			continue
		}
		if newerEvent(other, *event) {
			newer = true
			break
		}
		drop = append(drop, other.ID)
	}
	if newer {
		return db.DeletePending(ctx, event.ID)
	}
	for _, id := range drop {
		if err := db.DeletePending(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func newerEvent(next, current nostr.Event) bool {
	if next.CreatedAt != current.CreatedAt {
		return next.CreatedAt > current.CreatedAt
	}
	return next.ID < current.ID
}

func pendingHas(ctx context.Context, db store.T, id string) (bool, error) {
	var n int
	err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_events WHERE id = ?`, id).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// promoteListingsBy promotes the pending listings signed by any of pubkeys.
func (r *T) promoteListingsBy(ctx context.Context, pubkeys []string, skipBroadcast string) error {
	pending, err := r.store.ListPending(ctx, listingKinds())
	if err != nil {
		return err
	}
	seen := map[string]struct{}{}
	for _, event := range pending {
		appID, ok := listingAppID(&event)
		if !ok || !slices.Contains(pubkeys, event.PubKey) {
			continue
		}
		key := event.PubKey + ":" + appID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if _, err := r.promoteListing(ctx, event.PubKey, appID, skipBroadcast); err != nil {
			return err
		}
	}
	return nil
}

// promoteListing moves each ready listing of pubkey for appID from the buffer
// zone into events. skipBroadcast is the event the caller will broadcast
// itself. A release that can never be promoted is dropped; refused maps its
// event ids to the reason.
func (r *T) promoteListing(ctx context.Context, pubkey, appID, skipBroadcast string) (refused map[string]string, err error) {
	pending, err := r.store.PendingListing(ctx, appID)
	if err != nil {
		return nil, err
	}
	var pendingApp *nostr.Event
	var pendingReleases []nostr.Event
	pendingAssets := map[string]nostr.Event{}
	for i := range pending {
		ev := pending[i]
		if ev.PubKey != pubkey {
			continue
		}
		switch ev.Kind {
		case events.KindApp:
			if pendingApp == nil || newerEvent(ev, *pendingApp) {
				copy := ev
				pendingApp = &copy
			}
		case events.KindRelease:
			pendingReleases = append(pendingReleases, ev)
		case events.KindAsset:
			pendingAssets[ev.ID] = ev
		}
	}

	refused = map[string]string{}
	catalogKey := pubkey == r.config.Info.Pubkey
	var dev *nostr.Event
	if catalogKey {
		// While an active developer holds the app event, the catalog key
		// sends only releases that name it.
		if dev, err = r.developerApp(ctx, appID); err != nil {
			return nil, err
		}
		if dev != nil && pendingApp != nil {
			slog.Warn("listing refused", "event", pendingApp.ID, "pubkey", pubkey, "reason", msgDeveloperHoldsApp)
			if err := r.store.DeletePending(ctx, pendingApp.ID); err != nil {
				return nil, err
			}
			refused[pendingApp.ID] = msgDeveloperHoldsApp
			pendingApp = nil
		}
	}

	if pendingApp != nil && len(pendingReleases) == 0 && len(pendingAssets) == 0 {
		return r.promoteLoneApp(ctx, pubkey, appID, *pendingApp, skipBroadcast)
	}
	app, err := r.listingApp(ctx, pubkey, appID, pendingApp)
	if err != nil {
		return nil, err
	}

	releases := pendingReleases
	if len(releases) == 0 && len(pendingAssets) > 0 {
		// A pending asset can complete a release that is already stored. Load
		// only the releases that name one of these assets. The app's other
		// stored releases stay as they are.
		if releases, err = r.storedReleasesForAssets(ctx, pubkey, pendingAssets); err != nil {
			return nil, err
		}
	}
	for _, release := range releases {
		// A catalog release belongs to the developer's app event when it
		// names it. Its checks are the event checks only: nothing reads
		// the APK, and processing runs after promotion.
		releaseApp := app
		if catalogKey {
			var reason string
			if dev != nil && !namesApp(release, dev.PubKey, appID) {
				reason = msgDeveloperHoldsApp
			} else if releaseApp, reason, err = r.catalogReleaseApp(ctx, app, release, appID); err != nil {
				return nil, err
			}
			if reason != "" {
				if err := r.refuseRelease(ctx, release, refuse("%s", reason), pendingAssets, refused); err != nil {
					return nil, err
				}
				continue
			}
			if releaseApp == nil {
				continue
			}
		}
		check, err := r.checkRelease(ctx, pubkey, appID, releaseApp, release, pendingAssets)
		if err != nil {
			return nil, err
		}
		if check.refused != "" {
			if err := r.refuseRelease(ctx, release, check, pendingAssets, refused); err != nil {
				return nil, err
			}
			continue
		}
		if check.assets == nil {
			continue
		}
		if !catalogKey {
			refusal, done, err := r.processed(ctx, appID, releaseApp, release, check.assets)
			if err != nil {
				return nil, err
			}
			if !done {
				continue
			}
			if refusal != "" {
				if err := r.refuseRelease(ctx, release, refuse("%s", refusal), pendingAssets, refused); err != nil {
					return nil, err
				}
				continue
			}
		}
		assets := check.assets
		if err := r.commitListing(ctx, releaseApp, &release, check, pendingAssets, skipBroadcast); err != nil {
			return nil, err
		}
		if catalogKey {
			// Processing may refuse a catalog release later, and then it
			// is removed. A refusal already known is applied now.
			if err := r.processCatalog(ctx, appID, releaseApp, release, assets, refused); err != nil {
				return nil, err
			}
		}
		if app, err = r.listingApp(ctx, pubkey, appID, nil); err != nil {
			return nil, err
		}
	}
	return refused, nil
}

func (r *T) listingApp(ctx context.Context, pubkey, appID string, pending *nostr.Event) (*nostr.Event, error) {
	if pending != nil {
		return pending, nil
	}
	event, ok, err := loadEvent(ctx, r.store, `
		SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM events e
		JOIN tags t ON t.event_id = e.id
		WHERE e.kind = ? AND e.pubkey = ? AND t.key = 'd' AND t.value = ?
		LIMIT 1
	`, events.KindApp, pubkey, appID)
	if err != nil || !ok {
		return nil, err
	}
	return &event, nil
}

// msgDeveloperHoldsApp refuses a catalog event that would replace the app
// event of the app's active developer. Switching the app event back and forth
// would split the comments and zaps written under each coordinate.
const msgDeveloperHoldsApp = "the app has a developer's app event: send only a release that names it in its a tag"

// namesApp reports a release whose a tag is the kind 32267 of pubkey for appID.
func namesApp(release nostr.Event, pubkey, appID string) bool {
	got, ok := events.Find(release.Tags, "a")
	return ok && got == strconv.Itoa(events.KindApp)+":"+pubkey+":"+appID
}

// processCatalog queues the processing of a promoted catalog release, which
// can no longer hold it back. A refusal removes the release and its assets.
func (r *T) processCatalog(ctx context.Context, appID string, app *nostr.Event, release nostr.Event, assets []nostr.Event, refused map[string]string) error {
	refusal, _, err := r.processed(ctx, appID, app, release, assets)
	if err != nil || refusal == "" {
		return err
	}
	byID := map[string]nostr.Event{}
	for _, asset := range assets {
		byID[asset.ID] = asset
	}
	return r.dropCatalogRelease(ctx, app, release, byID, refusal, refused)
}

// storedReleasesForAssets returns stored releases that name one of assets.
// A pending asset whose release is already stored is promoted with that
// release. Releases that do not name the asset are left alone.
func (r *T) storedReleasesForAssets(ctx context.Context, pubkey string, assets map[string]nostr.Event) ([]nostr.Event, error) {
	if len(assets) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(assets)+2)
	args = append(args, events.KindRelease, pubkey)
	for id := range assets {
		args = append(args, id)
	}
	rows, err := r.store.DB.QueryContext(ctx, `
		SELECT DISTINCT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM events e
		JOIN tags t ON t.event_id = e.id
		WHERE e.kind = ? AND e.pubkey = ? AND t.key = 'e' AND t.value`+inClause(len(assets)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []nostr.Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

// promoteLoneApp promotes a kind 32267 sent without a release, as an edit of
// the app's metadata. The relay must already hold a release of the app, and
// the signer must be the active owner or delegate. The edit replaces the
// kind 32267 of every other signer. Until both hold it stays in the buffer
// zone.
func (r *T) promoteLoneApp(ctx context.Context, pubkey, appID string, app nostr.Event, skipBroadcast string) (map[string]string, error) {
	refused := map[string]string{}
	var one int
	err := r.store.DB.QueryRowContext(ctx, `
		SELECT 1 FROM events e JOIN tags t ON t.event_id = e.id AND t.key = 'i' AND t.value = ?
		WHERE e.kind = ? LIMIT 1`, appID, events.KindRelease).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return refused, nil
	}
	if err != nil {
		return nil, err
	}
	holds := pubkey == r.config.Info.Pubkey
	if !holds {
		holds, err = isActiveDeveloper(ctx, r.store, r.config.Info.Pubkey, appID, pubkey)
		if err != nil || !holds {
			return refused, err
		}
	}
	reason, err := r.bannedListing(ctx, []nostr.Event{app})
	if err != nil {
		return nil, err
	}
	if reason != "" {
		refused[app.ID] = reason
		return refused, r.store.DeletePending(ctx, app.ID)
	}
	stored, err := r.store.Replace(ctx, &app)
	if err != nil {
		return nil, err
	}
	if !stored {
		return refused, r.store.DeletePending(ctx, app.ID)
	}
	others, err := appIDsOf(ctx, r.store.DB, `
		SELECT e.id FROM events e JOIN tags t ON t.event_id = e.id AND t.key = 'd' AND t.value = ?
		WHERE e.kind = ? AND e.pubkey != ?`, appID, events.KindApp, pubkey)
	if err != nil {
		return nil, err
	}
	if len(others) > 0 {
		if _, err := r.store.Delete(ctx, nostr.Filter{IDs: others}); err != nil {
			return nil, fmt.Errorf("replace app event of %s: %w", appID, err)
		}
	}
	if err := r.store.DeletePending(ctx, app.ID); err != nil {
		return nil, err
	}
	if r.server != nil && app.ID != skipBroadcast {
		if err := r.server.Broadcast(&app); err != nil {
			slog.Error("relay: failed to broadcast app event", "event", app.ID, "error", err)
		}
	}
	r.SyncSearch(ctx, appID)
	if r.profileJobs != nil {
		r.enqueueProfile(app.PubKey)
	}
	slog.Info("app event replaced", "pubkey", pubkey, "app_id", appID)
	return refused, nil
}

// releaseCheck is the outcome of judging one release. assets is set when the
// listing is ready; refused is set when it can never be promoted. Neither
// means it waits.
type releaseCheck struct {
	assets []nostr.Event
	proofs []nostr.Event // checked pending proofs promoted with the listing
	cert   string        // the app certificate, pinned at promotion
	// owner and delegate are the active C1 pubkeys for cert, for a developer release.
	owner, delegate string
	refused         string
}

func refuse(format string, args ...any) releaseCheck {
	return releaseCheck{refused: fmt.Sprintf(format, args...)}
}

// checkRelease judges one release for promotion. The triple must be complete
// and, for a developer, every APK read, from Blossom, the APK cache, or the
// asset's url. No store may have signed it. The catalog key's APK is not read:
// the indexer verified it, and an asset without a url waits for its file in
// Blossom. A developer APK must be signed by the app's
// certificate or a successor in its lineage, and published by the active C1
// owner or delegate, counting pending proofs whose d tag names it. Signature
// verification of those proofs is off until relay-c1-check. Every release
// must pass the version guard.
func (r *T) checkRelease(ctx context.Context, pubkey, appID string, app *nostr.Event, release nostr.Event, pendingAssets map[string]nostr.Event) (releaseCheck, error) {
	if app == nil {
		return releaseCheck{}, nil
	}
	want := strconv.Itoa(events.KindApp) + ":" + pubkey + ":" + appID
	if got, ok := events.Find(release.Tags, "a"); ok && got != want {
		// The catalog key's release may name the developer's kind 32267.
		named := strconv.Itoa(events.KindApp) + ":" + app.PubKey + ":" + appID
		if pubkey != r.config.Info.Pubkey || app.PubKey == pubkey || got != named {
			return releaseCheck{}, nil
		}
	}
	parsed, err := events.ParseRelease(&release)
	if err != nil || len(parsed.AssetIDs) == 0 {
		return releaseCheck{}, nil
	}
	var assets []nostr.Event
	for _, id := range parsed.AssetIDs {
		asset, found, err := r.lookupAsset(ctx, id, pendingAssets)
		if err != nil {
			return releaseCheck{}, err
		}
		if !found || asset.PubKey != pubkey || asset.Kind != events.KindAsset {
			return releaseCheck{}, nil
		}
		if appIDTag, ok := events.Find(asset.Tags, "i"); !ok || appIDTag != appID {
			return releaseCheck{}, nil
		}
		assets = append(assets, asset)
	}
	if reason, err := r.bannedListing(ctx, slices.Concat([]nostr.Event{*app, release}, assets)); err != nil || reason != "" {
		if err != nil {
			return releaseCheck{}, err
		}
		return refuse("%s", reason), nil
	}
	check := releaseCheck{assets: assets}
	stored, err := r.admitted(ctx, release, assets, pendingAssets)
	if err != nil {
		return releaseCheck{}, err
	}
	if stored {
		return check, nil
	}
	if pubkey == r.config.Info.Pubkey {
		signer := assetSigner(assets[0])
		if signer == "" {
			return refuse("the asset names no apk_certificate_hash"), nil
		}
		for i := range assets {
			if r.assetURL(assets[i].Tags) != "" {
				continue
			}
			ready, err := r.assetInBlossom(ctx, &assets[i])
			if err != nil || !ready {
				return releaseCheck{}, err
			}
		}
		check.cert = signer
	} else {
		check, err = r.checkDeveloper(ctx, pubkey, appID, assets, pendingAssets)
		if err != nil || check.assets == nil {
			return check, err
		}
	}

	if pubkey == r.config.Info.Pubkey {
		if check.owner, check.delegate, err = r.activeDevelopers(ctx, appID); err != nil {
			return releaseCheck{}, err
		}
	}
	code, floor, err := r.versionGuard(ctx, pubkey, appID, release, assets, []string{r.config.Info.Pubkey, check.owner, check.delegate})
	if err != nil {
		return releaseCheck{}, err
	}
	if code < floor {
		return refuse("version_code %d is lower than %d, the current release in this channel", code, floor), nil
	}
	return check, nil
}

// admitted reports a release that is stored with all of its assets: it
// passed its checks when it was promoted, so none run again.
func (r *T) admitted(ctx context.Context, release nostr.Event, assets []nostr.Event, pendingAssets map[string]nostr.Event) (bool, error) {
	for _, asset := range assets {
		if _, ok := pendingAssets[asset.ID]; ok {
			return false, nil
		}
	}
	kept, err := pendingHas(ctx, r.store, release.ID)
	return !kept, err
}

// listedByOther reports a stored asset outside except that names x. The
// assets of the release being judged are excluded, so storing them first
// does not count as an earlier listing.
func (r *T) listedByOther(ctx context.Context, x string, except map[string]struct{}) (bool, error) {
	if x == "" {
		return false, nil
	}
	rows, err := r.store.DB.QueryContext(ctx, `
		SELECT e.id FROM events e
		JOIN tags t ON t.event_id = e.id
		WHERE e.kind = ? AND t.key = 'x' AND t.value = ?
	`, events.KindAsset, x)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return false, err
		}
		if _, ok := except[id]; ok {
			continue
		}
		return true, nil
	}
	return false, rows.Err()
}

// checkDeveloper reads the APK of each pending asset of a developer release
// and checks it against the certificate on the current asset, or for a new
// app the signer of its APK. A stored asset was read when it was promoted.
func (r *T) checkDeveloper(ctx context.Context, pubkey, appID string, assets []nostr.Event, pendingAssets map[string]nostr.Event) (releaseCheck, error) {
	appCert, _, err := appCertificate(ctx, r.store, r.config.Info.Pubkey, appID)
	if err != nil {
		return releaseCheck{}, err
	}
	var chain signing
	for _, asset := range assets {
		if _, ok := pendingAssets[asset.ID]; !ok {
			continue
		}
		x, _ := events.Find(asset.Tags, "x")
		got, readErr, ok := r.apkSigning(x, r.assetURL(asset.Tags))
		if !ok {
			return releaseCheck{}, nil
		}
		if readErr != "" {
			return refuse("%s", readErr), nil
		}
		if appCert == "" {
			appCert = got.signer()
		}
		if got.cert(appCert) == nil {
			return refuse("the APK is not signed by the app's certificate or a successor in its lineage"), nil
		}
		if !claimMatches(asset, got) {
			return refuse("the asset lineage does not match the APK"), nil
		}
		if !assetNames(asset, appCert) {
			return refuse("%s", errCertificate), nil
		}
		chain = signing{got.cert(appCert)}
	}
	if appCert == "" {
		return releaseCheck{}, nil
	}

	proofs, err := r.checkedProofs(ctx, chain)
	if err != nil {
		return releaseCheck{}, err
	}
	owner, delegate, ok, err := activeDeveloper(ctx, r.store, []string{appCert}, proofs)
	if err != nil {
		return releaseCheck{}, err
	}
	if !ok || (pubkey != owner && pubkey != delegate) {
		return releaseCheck{}, nil
	}
	return releaseCheck{assets: assets, proofs: proofs, cert: appCert, owner: owner, delegate: delegate}, nil
}

// versionGuard returns the release's version_code and the floor: the greatest
// version_code promoted in the same channel by the catalog key, the active
// owner, or the delegate, all in authorized, excluding this release. A former
// owner's release does not raise the floor. Only releases dated no later than
// this one count: a lower version_code dated before a higher one is an older
// link of the chain, not a downgrade, so judging it again never removes it.
func (r *T) versionGuard(ctx context.Context, pubkey, appID string, release nostr.Event, assets []nostr.Event, authorized []string) (code, floor int64, err error) {
	code = maxVersionCode(assets)
	channel, _ := events.Find(release.Tags, "c")
	d, _ := events.Find(release.Tags, "d")
	stored, err := r.storedReleaseCodes(ctx, appID)
	if err != nil {
		return 0, 0, err
	}
	for _, other := range stored {
		if other.channel != channel || !slices.Contains(authorized, other.pubkey) {
			continue
		}
		if other.pubkey == pubkey && other.d == d {
			continue
		}
		if other.at > int64(release.CreatedAt) {
			continue
		}
		floor = max(floor, other.code)
	}
	return code, floor, nil
}

func maxVersionCode(assets []nostr.Event) int64 {
	var out int64
	for _, asset := range assets {
		raw, _ := events.Find(asset.Tags, "version_code")
		code, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && code > out {
			out = code
		}
	}
	return out
}

// refuseRelease drops a release that can never be promoted, with its pending
// assets. The reason is logged and kept for the caller.
func (r *T) refuseRelease(ctx context.Context, release nostr.Event, check releaseCheck, pendingAssets map[string]nostr.Event, refused map[string]string) error {
	slog.Warn("listing refused", "release", release.ID, "pubkey", release.PubKey, "reason", check.refused)
	ids := []string{release.ID}
	for _, id := range events.FindAll(release.Tags, "e") {
		if _, ok := pendingAssets[id]; ok {
			ids = append(ids, id)
			delete(pendingAssets, id)
		}
	}
	for _, id := range ids {
		if err := r.store.DeletePending(ctx, id); err != nil {
			return err
		}
		refused[id] = check.refused
	}
	return nil
}

func (r *T) lookupAsset(ctx context.Context, id string, pending map[string]nostr.Event) (nostr.Event, bool, error) {
	if asset, ok := pending[id]; ok {
		return asset, true, nil
	}
	return loadEvent(ctx, r.store, `
		SELECT id, pubkey, created_at, kind, tags, content, sig FROM events WHERE id = ?
	`, id)
}

// bannedListing runs the blacklist again at promotion, since a ban can land
// while a listing waits in the buffer zone. It returns the refusal for the
// first member that a banned publisher or file hash rejects, or "".
// A nil policy store bans nothing.
func (r *T) bannedListing(ctx context.Context, members []nostr.Event) (string, error) {
	if r.pol == nil {
		return "", nil
	}
	for i := range members {
		verdict, err := r.pol.EventVerdict(ctx, &members[i])
		if err != nil {
			return "", err
		}
		if verdict.Reason != "" {
			return ErrFileNotAllowed.Error(), nil
		}
		if verdict.Reject {
			return ErrPublisherNotAllowed.Error(), nil
		}
	}
	return "", nil
}

func (r *T) assetInBlossom(ctx context.Context, asset *nostr.Event) (bool, error) {
	x, ok := events.Find(asset.Tags, "x")
	if !ok {
		return false, nil
	}
	hash, err := blossom.ParseHash(x)
	if err != nil {
		return false, nil
	}
	if r.blossom == nil {
		return false, errors.New("blossom is not configured")
	}
	return r.blossom.Has(ctx, hash)
}

// commitListing promotes a ready listing: its files are served, its pending
// events are stored and broadcast, the app certificate is pinned, and the
// proofs checked with it are stored.
func (r *T) commitListing(ctx context.Context, app *nostr.Event, release *nostr.Event, check releaseCheck, pendingAssets map[string]nostr.Event, skipBroadcast string) error {
	for _, hash := range listingFiles(app, check.assets) {
		if err := r.blossom.Promote(ctx, hash); err != nil {
			return fmt.Errorf("promote file %s: %w", hash, err)
		}
	}

	type member struct {
		event   nostr.Event
		pending bool
	}
	var members []member
	if app != nil {
		kept, err := pendingHas(ctx, r.store, app.ID)
		if err != nil {
			return err
		}
		members = append(members, member{event: *app, pending: kept})
	}
	if release != nil {
		kept, err := pendingHas(ctx, r.store, release.ID)
		if err != nil {
			return err
		}
		members = append(members, member{event: *release, pending: kept})
	}
	for _, asset := range check.assets {
		_, pending := pendingAssets[asset.ID]
		members = append(members, member{event: asset, pending: pending})
	}

	var saved []nostr.Event
	var anyPending bool
	for _, member := range members {
		if !member.pending {
			continue
		}
		anyPending = true
		event := member.event
		var stored bool
		var err error
		switch event.Kind {
		case events.KindApp, events.KindRelease:
			stored, err = r.store.Replace(ctx, &event)
		default:
			stored, err = r.store.Save(ctx, &event)
			if err == nil {
				stored = true
			}
		}
		if err != nil {
			return err
		}
		if !stored {
			if err := r.store.DeletePending(ctx, event.ID); err != nil {
				return err
			}
			continue
		}
		saved = append(saved, event)
	}
	if !anyPending {
		return nil
	}
	for _, proof := range check.proofs {
		if err := r.commitProof(ctx, proof, skipBroadcast); err != nil {
			return err
		}
	}
	// The pending rows stay until the superseded events are gone, so a
	// failure here is retried by the next reconcile.
	// Another signer's app event goes only when this listing's own app event
	// replaces it. A catalog release that names the developer's keeps it.
	keepApp := ""
	if app != nil && app.PubKey == release.PubKey {
		keepApp = release.PubKey
	}
	if err := r.supersede(ctx, release, keepApp); err != nil {
		return err
	}
	for _, event := range saved {
		if err := r.store.DeletePending(ctx, event.ID); err != nil {
			return err
		}
		if event.Kind == events.KindAsset {
			delete(pendingAssets, event.ID)
		}
		if r.server != nil && event.ID != skipBroadcast {
			if err := r.server.Broadcast(&event); err != nil {
				slog.Error("relay: failed to broadcast listing", "event", event.ID, "error", err)
			}
		}
	}
	if len(saved) == 0 {
		return nil
	}

	appID := listingAppIDOrEmpty(release)
	r.SyncSearch(ctx, appID)
	slog.Info("listing promoted", "pubkey", release.PubKey, "app_id", appID)
	for _, event := range saved {
		if event.Kind == events.KindApp && r.profileJobs != nil {
			r.enqueueProfile(event.PubKey)
		}
	}
	return nil
}

// listingFiles are the Blossom files a listing names: each asset's x tag and
// the app's icon and images.
func listingFiles(app *nostr.Event, assets []nostr.Event) []blossom.Hash {
	var values []string
	for _, asset := range assets {
		values = append(values, events.FindAll(asset.Tags, "x")...)
	}
	if app != nil {
		values = append(values, events.FindAll(app.Tags, "icon")...)
		values = append(values, events.FindAll(app.Tags, "image")...)
	}
	seen := map[string]struct{}{}
	var out []blossom.Hash
	for _, value := range values {
		for _, raw := range fileHash.FindAllString(strings.ToLower(value), -1) {
			if _, ok := seen[raw]; ok {
				continue
			}
			seen[raw] = struct{}{}
			if hash, err := blossom.ParseHash(raw); err == nil {
				out = append(out, hash)
			}
		}
	}
	return out
}

func listingAppIDOrEmpty(event *nostr.Event) string {
	id, _ := listingAppID(event)
	return id
}

func (r *T) dropPendingApp(ctx context.Context, appID string) (int, error) {
	return r.dropPending(ctx, func(event nostr.Event) bool {
		id, ok := listingAppID(&event)
		return ok && id == appID
	})
}

func (r *T) dropPendingRefs(ctx context.Context, event *nostr.Event, pubkey string) (int, error) {
	ids := map[string]struct{}{}
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			ids[tag[1]] = struct{}{}
		}
	}
	var coords []events.AddressableRef
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != "a" {
			continue
		}
		ref, err := events.ParseAddressableRef(tag[1])
		if err != nil {
			continue
		}
		coords = append(coords, ref)
	}
	return r.dropPending(ctx, func(candidate nostr.Event) bool {
		if pubkey != "" && candidate.PubKey != pubkey {
			return false
		}
		if _, ok := ids[candidate.ID]; ok {
			return true
		}
		d, ok := events.Find(candidate.Tags, "d")
		if !ok {
			return false
		}
		for _, ref := range coords {
			if candidate.Kind == ref.Kind && candidate.PubKey == ref.Pubkey && d == ref.DTag {
				return true
			}
		}
		return false
	})
}

func (r *T) dropPending(ctx context.Context, match func(nostr.Event) bool) (int, error) {
	pending, err := r.store.ListPending(ctx, bufferedKinds())
	if err != nil {
		return 0, err
	}
	var n int
	for _, event := range pending {
		if !match(event) {
			continue
		}
		if err := r.store.DeletePending(ctx, event.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func loadEvent(ctx context.Context, db store.T, query string, args ...any) (nostr.Event, bool, error) {
	row := db.DB.QueryRowContext(ctx, query, args...)
	event, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nostr.Event{}, false, nil
	}
	if err != nil {
		return nostr.Event{}, false, err
	}
	return event, true, nil
}

type eventScanner interface {
	Scan(dest ...any) error
}

func scanEvent(row eventScanner) (nostr.Event, error) {
	var event nostr.Event
	var created int64
	var tags string
	if err := row.Scan(&event.ID, &event.PubKey, &created, &event.Kind, &tags, &event.Content, &event.Sig); err != nil {
		return nostr.Event{}, err
	}
	event.CreatedAt = nostr.Timestamp(created)
	if err := event.Tags.Scan(tags); err != nil {
		return nostr.Event{}, err
	}
	return event, nil
}
