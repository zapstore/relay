// The relay package is responsible for setting up the relay.
// It exposes a [Setup] function to create a new relay with the given config.
package relay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/blossom"
	"github.com/pippellia-btc/rely/v2"
	defender "github.com/zapstore/defender/pkg/client"
	"github.com/zapstore/defender/pkg/models"
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/events/legacy"
	"github.com/zapstore/relay/pkg/indexing"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay/store"
)

var (
	ErrEventKindNotAllowed = errors.New("event kind is not in the allowed list")
	ErrEventPubkeyBlocked  = errors.New("event pubkey is not allowed. Visit https://zapstore.dev/docs/publish for more information.")

	ErrAppAlreadyExists = errors.New(`failed to publish app: another pubkey has already published an app with the same 'd' tag identifier.
		This is a precautionary measure because Android doesn't allow apps with the same identifier to be installed side by side.
		Please use a different identifier or contact the Zapstore team for more information.`)

	ErrTooManyFilters  = errors.New("number of filters exceed the maximum allowed per REQ")
	ErrFiltersTooVague = errors.New("filters are too vague")

	ErrInternal    = errors.New("internal error, please contact the Zapstore team.")
	ErrRateLimited = errors.New("rate-limited: slow down chief")
)

// RootKinds are the ones that can be published without referencing a parent event.
var RootKinds = []int{
	events.KindForumPost,
	events.KindApp,
	events.KindStack,
}

// indexerPubkeyFallback is the hardcoded zapstore indexer pubkey used when RELAY_PUBKEY is not set.
const indexerPubkeyFallback = "78ce6faa72264387284e647ba6938995735ec8c7d5c5a65737e55130f026307d"

// T represents the relay and all its dependencies.
type T struct {
	server *rely.Relay
	config Config

	limiter   rate.Limiter
	defender  defender.T
	store     store.T
	analytics *analytics.Engine
	indexing  *indexing.Engine

	blossom Blossom
	uploads chan upload
}

type upload struct {
	hash blossom.Hash
	mime string
}

// Blossom is an interface that represents the subset of the blossom functionalities needed by the relay.
type Blossom interface {
	// Has returns whether a hash exists in the blossom database.
	Has(ctx context.Context, hash blossom.Hash) (bool, error)
}

// Setup creates a new relay instance with the given dependencies and configuration.
// It registers all functions to the rely.Relay hooks.
func Setup(
	config Config,
	limiter rate.Limiter,
	defender defender.T,
	store store.T,
	blssm Blossom,
	analytics *analytics.Engine,
	indexing *indexing.Engine,
) (*T, error) {

	server := rely.NewRelay(
		rely.WithAuthURL(config.Hostname),
		rely.WithInfo(config.Info.NIP11()),
		rely.WithQueueCapacity(config.QueueCapacity),
		rely.WithMaxMessageSize(config.MaxMessageBytes),
		rely.WithClientResponseLimit(config.ResponseLimit),
	)

	server.Reject.Connection.Clear()
	server.Reject.Connection.Append(
		RateConnectionIP(limiter),
		rely.RegistrationFailWithin(3*time.Second),
	)

	server.Reject.Event.Clear()
	server.Reject.Event.Append(
		RateEventIP(limiter),
		KindNotAllowed(config.AllowedKinds),
		rely.InvalidID,
		rely.InvalidSignature,
		InvalidStructure,
		NotAnchored(store),
		NotAllowed(defender),
		AppOwnership(store, config.Info.Pubkey),
	)

	server.Reject.Req.Clear()
	server.Reject.Req.Append(
		RateReqIP(limiter),
		FiltersExceed(config.MaxReqFilters),
		VagueFilters(3),
	)

	relay := &T{
		server: server,
		config: config,

		limiter:   limiter,
		defender:  defender,
		store:     store,
		analytics: analytics,
		indexing:  indexing,

		blossom: blssm,
		uploads: make(chan upload, 100),
	}

	server.On.Event = relay.save
	server.On.Req = relay.query
	return relay, nil
}

// StartAndServe starts the relay, listens to the provided address and handles http requests.
func (r *T) StartAndServe(ctx context.Context, addr string) error {
	go r.runReconcile(ctx)
	go r.runStater(ctx)
	return r.server.StartAndServe(ctx, addr)
}

// NotifyUpload notifies the relay that the upload of the blob with the given hash and mime type is complete.
// The signal is used to trigger the reconciliation of pending events.
func (r *T) NotifyUpload(hash blossom.Hash, mime string) error {
	select {
	case r.uploads <- upload{hash: hash, mime: mime}:
		return nil
	default:
		return errors.New("channel is full")
	}
}

// ResolveAssetURL looks up a download URL for a kind 3063 asset by the SHA-256 hash in its "x" tag.
func (r *T) ResolveAssetURL(ctx context.Context, hash blossom.Hash) (string, error) {
	filter := nostr.Filter{
		Kinds: []int{events.KindAsset},
		Tags:  nostr.TagMap{"x": []string{hash.Hex()}},
		Limit: 1,
	}
	found, err := r.store.Query(ctx, filter)
	if err != nil || len(found) == 0 {
		return "", err
	}
	url, _ := events.Find(found[0].Tags, "url")
	return url, nil
}

// TODO: this logs stats. Remove when done debugging
func (r *T) runStater(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			clients := r.server.Clients()
			subs := r.server.Subscriptions()
			filters := r.server.Filters()
			queueLoad := r.server.QueueLoad()
			slog.Debug("stats report", "clients", clients, "subs", subs, "filters", filters, "queue_load", queueLoad)
		}
	}
}

func (r *T) runReconcile(ctx context.Context) {
	ticker := time.NewTicker(r.config.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			err := r.reconcile(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("reconcile failed", "error", err)
			}

		case u := <-r.uploads:
			if u.mime == "application/vnd.android.package-archive" {
				// because assets are supposed to reference APKs in their "x" tags,
				// reconcile only when an APK is uploaded
				err := r.reconcile(ctx)
				if err != nil && !errors.Is(err, context.Canceled) {
					slog.Error("reconcile failed", "error", err)
				}
			}
		}
	}
}

// reconcile is responsible for checking whether to promote pending events to normal events
// so they can be served in queries.
func (r *T) reconcile(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	assets, err := r.store.QueryPending(ctx, events.KindAsset)
	if err != nil {
		return fmt.Errorf("failed to reconcile events: %w", err)
	}

	errs := make([]error, 0, len(assets))
	for _, asset := range assets {

		ready, err := isAssetReady(ctx, r.blossom, &asset)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to check if asset is ready: %w", err))
			continue
		}

		if ready {
			if _, err := r.store.Save(ctx, &asset); err != nil {
				errs = append(errs, fmt.Errorf("failed to save event %s: %w", asset.ID, err))
				continue
			}
			if err := r.store.DeletePending(ctx, asset.ID); err != nil {
				errs = append(errs, fmt.Errorf("failed to delete pending event %s: %w", asset.ID, err))
				continue
			}
			if err := r.server.Broadcast(&asset); err != nil {
				errs = append(errs, fmt.Errorf("failed to broadcast event %s: %w", asset.ID, err))
				continue
			}
		}
	}

	cutoff := time.Now().UTC().Add(-r.config.RemovePendingAfter)
	if err := r.store.DeleteExpiredPending(ctx, cutoff); err != nil {
		errs = append(errs, fmt.Errorf("failed to delete expired pending events: %w", err))
	}
	return errors.Join(errs...)
}

func (r *T) save(c rely.Client, event *nostr.Event) rely.EventResult {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r.analytics.RecordEvent(c, event)

	switch {
	case event.Kind == nostr.KindDeletion:
		if err := r.handleDelete(ctx, event); err != nil {
			slog.Error("relay: failed to fullfil delete", "event", event.ID, "error", err)
			return rely.Fail(err.Error())
		}

	case event.Kind == events.KindAsset:
		isPending, err := r.saveAsset(ctx, event)
		if err != nil {
			slog.Error("relay: failed to save asset event", "event", event.ID, "error", err)
			return rely.Fail(err.Error())
		}

		if isPending {
			// avoid broadcasting the event until it is fully saved
			return rely.Success().NoBroadcast().WithReply("the event will be saved when the referenced blob is uploaded")
		}

	case nostr.IsRegularKind(event.Kind):
		if _, err := r.store.Save(ctx, event); err != nil {
			slog.Error("relay: failed to save regular event", "event", event.ID, "error", err)
			return rely.Fail(err.Error())
		}

	case nostr.IsReplaceableKind(event.Kind) || nostr.IsAddressableKind(event.Kind):
		if _, err := r.store.Replace(ctx, event); err != nil {
			slog.Error("relay: failed to replace event", "event", event.ID, "error", err)
			return rely.Fail(err.Error())
		}
	}
	return rely.Success()
}

// handleDelete handles deletion events, either from the operator or a regular NIP-09 deletion.
func (r *T) handleDelete(ctx context.Context, event *nostr.Event) error {
	if event.Kind != nostr.KindDeletion {
		return errors.New("event is not a deletion")
	}

	var deleted int
	var err error

	if event.PubKey == r.config.Info.Pubkey {
		// Operator delete request
		deleted, err = r.store.ForceDeleteRequest(ctx, event)
		if err != nil {
			return fmt.Errorf("failed to perform operator delete: %w", err)
		}
	} else {
		// Regular NIP-09 deletion
		deleted, err = r.store.DeleteRequest(ctx, event)
		if err != nil {
			return fmt.Errorf("failed to perform delete: %w", err)
		}
	}

	// Reject deletion events that didn't delete anything
	if deleted == 0 {
		return nil
	}

	// Save the delete request. Clients will fetch these to remove the deleted events from their local cache.
	if _, err := r.store.Save(ctx, event); err != nil {
		return fmt.Errorf("failed to save delete request: %w", err)
	}
	return nil
}

// saveAsset saves an asset event to the store.
// If the asset references a blob that is not in blossom yet, it will be saved as pending, until the
// runReconcile loop saves it to the store or deletes it if too much time has passed.
func (r *T) saveAsset(ctx context.Context, event *nostr.Event) (isPending bool, err error) {
	if event.Kind != events.KindAsset {
		return false, errors.New("event is not an asset")
	}

	ready, err := isAssetReady(ctx, r.blossom, event)
	if err != nil {
		return false, fmt.Errorf("failed to check if asset is ready: %w", err)
	}

	if ready {
		if _, err := r.store.Save(ctx, event); err != nil {
			return false, fmt.Errorf("failed to save the asset event: %w", err)
		}
		return false, nil
	}

	// the asset reference a blob that is not yet in blossom. Save as pending
	if _, err := r.store.SavePending(ctx, event); err != nil {
		return false, fmt.Errorf("failed to save the asset event as pending: %w", err)
	}
	return true, nil
}

// assetChecker is used exclusively for HEAD requests in isAssetReady.
var assetChecker = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	},
}

// isAssetReady returns whether the asset's blob has been correctly uploaded.
// It first checks the local blossom database, and then falls back to checking all "url" tags in the event.
func isAssetReady(ctx context.Context, b Blossom, asset *nostr.Event) (bool, error) {
	if asset.Kind != events.KindAsset {
		return false, errors.New("event must be an asset event")
	}

	// first we check the local blossom database
	xTag, ok := events.Find(asset.Tags, "x")
	if !ok {
		return false, errors.New("asset doesn't have an 'x' tag")
	}

	hash, err := blossom.ParseHash(xTag)
	if err != nil {
		return false, fmt.Errorf("invalid x tag: %w", err)
	}

	found, err := b.Has(ctx, hash)
	if err != nil {
		return false, fmt.Errorf("failed to check hash: %w", err)
	}
	if found {
		return true, nil
	}

	// if the hash is not found locally, we check the "url" tags
	// by making a HEAD request for each URL
	urls := events.FindAll(asset.Tags, "url")
	if len(urls) == 0 {
		return false, nil
	}

	for _, url := range urls {
		if strings.HasPrefix(url, "https://cdn.zapstore.dev") {
			// skip URLs from the zapstore CDN, because they would have been already in the blossom db
			continue
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
		if err != nil {
			slog.Warn("isAssetReady: skipping malformed url tag", "event", asset.ID, "url", url, "error", err)
			continue
		}

		res, err := assetChecker.Do(req)
		if err != nil {
			slog.Warn("isAssetReady: skipping url", "event", asset.ID, "url", url, "error", err)
			continue
		}

		if res.StatusCode >= 200 && res.StatusCode < 300 {
			res.Body.Close()
			return true, nil
		}
		res.Body.Close()
	}
	return false, nil
}

func (r *T) query(ctx context.Context, c rely.Client, id string, filters nostr.Filters) ([]nostr.Event, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result, err := r.store.Query(ctx, filters...)
	if errors.Is(err, store.ErrUnsupportedREQ) {
		return nil, err
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("relay: failed to query events", "error", err, "filters", filters)
		return nil, err
	}

	if r.indexing != nil {
		recordDemandSignals(r.indexing, id, filters, result)
	}

	r.analytics.RecordReq(c, id, filters, result)
	return result, nil
}

// recordDemandSignals records discovery misses and release requests non-blocking.
// Release-request signals are gated to known Zapstore client subscription prefixes,
// filtering out bots and non-Zapstore clients.
func recordDemandSignals(idx *indexing.Engine, subID string, filters nostr.Filters, result []nostr.Event) {
	wantsReleases := strings.HasPrefix(subID, "app-updates") ||
		strings.HasPrefix(subID, "app-detail") ||
		strings.HasPrefix(subID, "app-bg") ||
		strings.HasPrefix(subID, "web-app-detail") ||
		strings.HasPrefix(subID, "web-releases")

	for _, filter := range filters {
		// Discovery miss: NIP-50 search on kind 32267 with a GitHub URL and zero results.
		// Not gated on subID — discovery misses are always meaningful.
		// DISABLED: GitHub/user-repo indexing temporarily commented out.
		// if filter.Search != "" && len(result) == 0 {
		// 	if isKindOnly(filter.Kinds, events.KindApp) {
		// 		idx.RecordDiscoveryMiss(filter.Search)
		// 	}
		// }

		if hasReleaseKind(filter.Kinds) {
			if !wantsReleases {
				continue
			}
			if len(result) > 0 {
				if iVals, ok := filter.Tags["i"]; ok {
					for _, appID := range iVals {
						idx.RecordReleaseRequest(appID)
					}
				} else {
					for _, ev := range result {
						if appID, ok := events.Find(ev.Tags, "i"); ok {
							idx.RecordReleaseRequest(appID)
						}
					}
				}
			}
		}
	}
}

func hasReleaseKind(kinds []int) bool {
	for _, k := range kinds {
		if k == events.KindRelease || k == events.KindAsset || k == legacy.KindFile {
			return true
		}
	}
	return false
}

func RateConnectionIP(limiter rate.Limiter) func(_ rely.Stats, request *http.Request) error {
	return func(_ rely.Stats, request *http.Request) error {
		cost := 1.0
		ip := rely.GetIP(request).Group()
		if !limiter.Allow(ip, cost) {
			slog.Debug("relay: rejecting connection", "ip", ip)
			return ErrRateLimited
		}
		return nil
	}
}

func RateEventIP(limiter rate.Limiter) func(client rely.Client, _ *nostr.Event) error {
	return func(client rely.Client, _ *nostr.Event) error {
		cost := 5.0
		ip := client.IP().Group()
		if !limiter.Allow(ip, cost) {
			client.Disconnect()
			slog.Debug("relay: rejecting event and disconnecting", "ip", ip)
			return ErrRateLimited
		}
		return nil
	}
}

func RateReqIP(limiter rate.Limiter) func(client rely.Client, id string, filters nostr.Filters) error {
	return func(client rely.Client, id string, filters nostr.Filters) error {
		ip := client.IP().Group()
		cost := 1.0
		if len(filters) > 10 {
			cost = 5.0
		}

		if !limiter.Allow(ip, cost) {
			client.Disconnect()
			slog.Debug("relay: rejecting req and disconnecting", "ip", ip)
			return ErrRateLimited
		}
		return nil
	}
}

func FiltersExceed(n int) func(_ rely.Client, _ string, filters nostr.Filters) error {
	return func(_ rely.Client, _ string, filters nostr.Filters) error {
		if len(filters) > n {
			return ErrTooManyFilters
		}
		return nil
	}
}

// VagueFilters rejects filters whose specificity score is below the given minimum.
// Set min to 0 to disable the check entirely.
func VagueFilters(min int) func(rely.Client, string, nostr.Filters) error {
	return func(_ rely.Client, _ string, filters nostr.Filters) error {
		if min <= 0 {
			return nil
		}
		for _, f := range filters {
			if specificity(f) < min {
				return ErrFiltersTooVague
			}
		}
		return nil
	}
}

// specificity estimates how specific a filter is, based on the presence of conditions.
// TODO: make it more accurate by considering what the conditions are (e.g. 1 kind vs 10 kinds).
func specificity(filter nostr.Filter) int {
	points := 0
	if len(filter.IDs) > 0 {
		points += 10
	}
	if filter.Search != "" {
		points += 3
	}
	if len(filter.Authors) > 0 {
		points += 2
	}
	if len(filter.Tags) > 0 {
		points += 2
	}
	if len(filter.Kinds) > 0 {
		points += 1
	}
	if filter.Since != nil {
		points += 1
	}
	if filter.Until != nil {
		points += 1
	}
	if filter.LimitZero {
		points += 5
	}
	if !filter.LimitZero && filter.Limit < 100 {
		points += 2
	}
	return points
}

func KindNotAllowed(kinds []int) func(_ rely.Client, e *nostr.Event) error {
	return func(_ rely.Client, e *nostr.Event) error {
		if !slices.Contains(kinds, e.Kind) {
			return fmt.Errorf("%w: %v", ErrEventKindNotAllowed, kinds)
		}
		return nil
	}
}

func InvalidStructure(_ rely.Client, e *nostr.Event) error {
	return events.Validate(e)
}

// AppOwnership enforces one publisher per app ID (kind 32267 d-tag) with role-aware transitions.
//
// Transition table:
//   - same pubkey → same pubkey:         accept (NIP-33 replace)
//   - indexer → non-indexer:             delete old 32267, accept (indexer takeover)
//   - non-indexer → indexer:             delete indexer's 32267, accept (developer reclaim)
//   - anyone else → anyone else:         reject
func AppOwnership(db store.T, indexerPubkey string) func(_ rely.Client, e *nostr.Event) error {
	if indexerPubkey == "" {
		indexerPubkey = indexerPubkeyFallback
	}
	return func(_ rely.Client, e *nostr.Event) error {
		if e.Kind != events.KindApp {
			return nil
		}

		appID, ok := events.Find(e.Tags, "d")
		if !ok {
			return nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		// Find any existing kind 32267 with this d-tag from a different pubkey
		query := `SELECT e.id, e.pubkey
					FROM events AS e JOIN tags AS t ON t.event_id = e.id
					WHERE e.kind = ?
					AND e.pubkey != ?
					AND t.key = 'd' AND t.value = ?
					LIMIT 1`

		var existingID, existingPubkey string
		err := db.DB.QueryRowContext(ctx, query, events.KindApp, e.PubKey, appID).Scan(&existingID, &existingPubkey)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			slog.Error("AppOwnership: failed to query existing app", "error", err)
			return ErrInternal
		}

		if existingID == "" {
			// No conflict: new app or a normal replacement by the same pubkey
			return nil
		}

		newIsIndexer := e.PubKey == indexerPubkey
		existingIsIndexer := existingPubkey == indexerPubkey

		switch {
		// case newIsIndexer && !existingIsIndexer:
		// 	// Indexer takeover: delete developer's old 32267
		// 	if err := deleteEvent(ctx, db, existingID); err != nil {
		// 		slog.Error("AppOwnership: failed to delete old app event for indexer takeover", "event_id", existingID, "error", err)
		// 		return ErrInternal
		// 	}
		// 	slog.Info("AppOwnership: indexer takeover", "app_id", appID, "old_pubkey", existingPubkey[:16])
		// 	return nil

		case existingIsIndexer && !newIsIndexer:
			// Developer reclaim: delete indexer's app event
			deleted, err := db.Delete(ctx, nostr.Filter{IDs: []string{existingID}})
			if err != nil {
				slog.Error("AppOwnership: failed to delete indexer app event for developer reclaim", "id", existingID, "error", err)
				return ErrInternal
			}
			if deleted > 0 {
				slog.Info("AppOwnership: developer reclaim", "app_id", appID, "new_pubkey", e.PubKey)
			}
			return nil

		default:
			// Non-indexer vs non-indexer: reject
			return ErrAppAlreadyExists
		}
	}
}

// NotAnchored returns an error if the event is not "anchored" to an existing event.
// Anchoring means simply that the event references an existing root event.
func NotAnchored(db store.T) func(_ rely.Client, e *nostr.Event) error {
	return func(_ rely.Client, e *nostr.Event) error {
		if slices.Contains(RootKinds, e.Kind) {
			// root events do not need to be "anchored" to an existing event
			return nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		switch e.Kind {
		case events.KindProfile:
			isPublisher, err := db.Has(ctx, nostr.Filter{Authors: []string{e.PubKey}})
			if err != nil {
				slog.Error("NotAnchored: failed to check kind:0", "err", err)
				return ErrInternal
			}

			if !isPublisher {
				return errors.New("kind 0: pubkey must have other events on this relay")
			}

		case events.KindComment:
			aTag, hasA := events.Find(e.Tags, "A")
			eTag, hasE := events.Find(e.Tags, "e")
			if !hasA && !hasE {
				return fmt.Errorf("kind 1111: must have an 'A' tag (root) or 'e' tag (reply)")
			}

			if hasA {
				// A tag must reference a known app kind:32267 or stack kind:30267 event
				ref, err := events.ParseAddressableRef(aTag)
				if err != nil {
					return fmt.Errorf("kind 1111: 'A' tag must reference a kind 32267 or kind 30267: %w", err)
				}
				if err := ref.Validate(); err != nil {
					return fmt.Errorf("kind 1111: 'A' tag must reference a kind 32267 or kind 30267: %w", err)
				}
				if ref.Kind != events.KindApp && ref.Kind != events.KindStack {
					return fmt.Errorf("kind 1111: 'A' tag must reference a kind 32267 or kind 30267: %d", ref.Kind)
				}

				found, err := db.Has(ctx, ref.Filter())
				if err != nil {
					slog.Error("NotAnchored: failed to check comment", "error", err, "event", e.ID, "tag", aTag)
					return ErrInternal
				}
				if !found {
					return fmt.Errorf("kind 1111: 'A' tag reference not found on this relay")
				}
			}

			if hasE {
				// e tag must reference a known kind:11 or kind:1111 event
				f := nostr.Filter{
					IDs:   []string{eTag},
					Kinds: []int{events.KindForumPost, events.KindComment},
				}

				found, err := db.Has(ctx, f)
				if err != nil {
					slog.Error("NotAnchored: failed to check comment", "error", err, "event", e.ID, "tag", eTag)
					return ErrInternal
				}
				if !found {
					return fmt.Errorf("kind 1111: 'e' tag reference not found on this relay")
				}
			}

		case events.KindZap:
			aTag, hasA := events.Find(e.Tags, "a")
			eTag, hasE := events.Find(e.Tags, "e")
			if !hasA && !hasE {
				return fmt.Errorf("kind 9735: must have an 'a' tag or 'e' tag")
			}

			if hasA {
				// a tag must reference a known app kind:32267 or stack kind:30267 event
				ref, err := events.ParseAddressableRef(aTag)
				if err != nil {
					return fmt.Errorf("kind 9735: 'a' tag must reference a kind 32267 or kind 30267: %w", err)
				}
				if err := ref.Validate(); err != nil {
					return fmt.Errorf("kind 9735: 'a' tag must reference a kind 32267 or kind 30267: %w", err)
				}
				if ref.Kind != events.KindApp && ref.Kind != events.KindStack {
					return fmt.Errorf("kind 9735: 'a' tag must reference a kind 32267 or kind 30267: %d", ref.Kind)
				}

				found, err := db.Has(ctx, ref.Filter())
				if err != nil {
					slog.Error("NotAnchored: failed to check zap", "error", err, "event", e.ID, "tag", aTag)
					return ErrInternal
				}
				if !found {
					return fmt.Errorf("kind 9735: 'a' tag reference not found on this relay")
				}
			}

			if hasE {
				// e tag must reference any known event
				found, err := db.Has(ctx, nostr.Filter{IDs: []string{eTag}})
				if err != nil {
					slog.Error("NotAnchored: failed to check zap", "error", err, "event", e.ID, "tag", eTag)
					return ErrInternal
				}

				if !found {
					return fmt.Errorf("kind 9735: e tag reference not found on this relay")
				}
			}
		}
		return nil
	}
}

func NotAllowed(defender defender.T) func(_ rely.Client, e *nostr.Event) error {
	return func(_ rely.Client, e *nostr.Event) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		res, err := defender.CheckEvent(ctx, e)
		if err != nil {
			slog.Error("defender: failed to check event", "err", err, "event", e.ID)
			return ErrInternal
		}
		if res.Decision == models.DecisionReject {
			return ErrEventPubkeyBlocked
		}
		return nil
	}
}
