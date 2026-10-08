// The relay package is responsible for setting up the relay.
// It exposes a [Setup] function to create a new relay with the given config.
package relay

import (
	"context"
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
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/policy"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay/store"
)

var (
	ErrEventKindNotAllowed = errors.New("event kind is not in the allowed list")
	ErrEventPubkeyBlocked  = errors.New("event pubkey is not allowed. Visit https://zapstore.dev/docs/publish for more information.")
	ErrPublisherNotAllowed = errors.New("publisher is not allowed. Visit https://zapstore.dev/docs/publish for more information.")
	ErrFileNotAllowed      = errors.New("file is not allowed. Visit https://zapstore.dev/docs/publish for more information.")

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

// DB is the relay database.
type DB = store.T

// NewDB creates a new relay database.
func NewDB(path string) (DB, error) {
	return store.New(path)
}

// T represents the relay and all its dependencies.
type T struct {
	server *rely.Relay
	config Config

	limiter   rate.Limiter
	store     store.T
	analytics *analytics.Engine

	blossom Blossom
	uploads chan upload

	profileJobs chan string
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
	pol *policy.Store,
	gate *policy.Gate,
	store store.T,
	blssm Blossom,
	analytics *analytics.Engine,
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
		NotARM64,
		NotAnchored(store, config.Info.Pubkey),
		NotAllowed(store, pol, blssm),
		NotAdmitted(gate),
		ListingSlots(store, config.Info.Pubkey),
		catalogKeyOnSocket(config.Info.Pubkey),
	)

	server.Reject.Req.Clear()
	server.Reject.Req.Append(
		RateReqIP(limiter),
		FiltersExceed(config.MaxReqFilters),
		UnsupportedQuery,
		VagueFilters(3),
	)

	relay := &T{
		server: server,
		config: config,

		limiter:   limiter,
		store:     store,
		analytics: analytics,

		blossom:     blssm,
		uploads:     make(chan upload, 100),
		profileJobs: make(chan string, 100),
	}

	server.On.Event = relay.save
	server.On.Req = relay.query
	return relay, nil
}

// Handler is the WebSocket relay. Mount it at "/" beside other routes.
func (r *T) Handler() http.Handler { return r.server }

// StartAndServe starts the relay. handler receives every request; nil serves only the WebSocket relay.
func (r *T) StartAndServe(ctx context.Context, addr string, handler http.Handler) error {
	go r.runReconcile(ctx)
	go r.runProfileWorker(ctx)
	r.server.Start(ctx)
	if handler == nil {
		handler = r.server
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	exit := make(chan error, 1)
	go func() {
		slog.Info("serving the relay", "address", addr)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			exit <- err
		}
	}()
	select {
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := srv.Shutdown(shut)
		r.server.Wait()
		return err
	case err := <-exit:
		return err
	}
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
			r.replaceOtherDeveloper(ctx, &asset)
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
	return r.persist(c, event).result()
}

type persistResult struct {
	ok        bool
	broadcast bool
	msg       string
}

func (p persistResult) result() rely.EventResult {
	if !p.ok {
		return rely.Fail(p.msg)
	}
	res := rely.Success()
	if !p.broadcast {
		res = res.NoBroadcast()
	}
	if p.msg != "" {
		res = res.WithReply(p.msg)
	}
	return res
}

func (r *T) persist(c rely.Client, event *nostr.Event) persistResult {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if r.analytics != nil {
		r.analytics.RecordEvent(c, event)
	}

	switch {
	case event.Kind == nostr.KindDeletion:
		if err := r.handleDelete(ctx, event); err != nil {
			slog.Error("relay: failed to fullfil delete", "event", event.ID, "error", err)
			return persistResult{msg: err.Error()}
		}

	case event.Kind == events.KindAsset:
		isPending, err := r.saveAsset(ctx, event)
		if err != nil {
			slog.Error("relay: failed to save asset event", "event", event.ID, "error", err)
			return persistResult{msg: err.Error()}
		}
		if isPending {
			return persistResult{ok: true, msg: "the event will be saved when the referenced blob is uploaded"}
		}

	case nostr.IsRegularKind(event.Kind):
		if _, err := r.store.Save(ctx, event); err != nil {
			slog.Error("relay: failed to save regular event", "event", event.ID, "error", err)
			return persistResult{msg: err.Error()}
		}

	case nostr.IsReplaceableKind(event.Kind) || nostr.IsAddressableKind(event.Kind):
		saved, err := r.store.Replace(ctx, event)
		if err != nil {
			slog.Error("relay: failed to replace event", "event", event.ID, "error", err)
			return persistResult{msg: err.Error()}
		}
		if saved && event.Kind == events.KindApp {
			r.enqueueProfile(event.PubKey)
		}
		if saved && event.Kind == events.KindRelease {
			r.replaceOtherDeveloper(ctx, event)
		}
	}
	return persistResult{ok: true, broadcast: true}
}

// handleDelete handles deletion events, either from the operator or a regular NIP-09 deletion.
func (r *T) handleDelete(ctx context.Context, event *nostr.Event) error {
	if event.Kind != nostr.KindDeletion {
		return errors.New("event is not a deletion")
	}

	var deleted int
	var err error

	switch {
	case event.PubKey == r.config.Info.Pubkey:
		deleted, err = r.store.ForceDeleteRequest(ctx, event)
		if err != nil {
			return fmt.Errorf("failed to perform operator delete: %w", err)
		}
	case r.isDeveloperRemoval(ctx, event):
		deleted, err = r.deleteAppListings(ctx, developerRemovalApp(event))
		if err != nil {
			return err
		}
	default:
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
		r.replaceOtherDeveloper(ctx, event)
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

	if r.analytics != nil {
		r.analytics.RecordReq(c, id, filters, result)
	}
	return result, nil
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
		if client.UID() == "nostr.sock" {
			return nil
		}
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
		if client.UID() == "nostr.sock" {
			return nil
		}
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

const platformARM64 = "android-arm64-v8a"

// NotARM64 rejects an app, release, asset, or stack that does not include android-arm64-v8a.
// Other f tags may be present. Other kinds pass.
func NotARM64(_ rely.Client, e *nostr.Event) error {
	switch e.Kind {
	case events.KindApp, events.KindRelease, events.KindAsset, events.KindStack:
	default:
		return nil
	}
	for _, tag := range e.Tags {
		if len(tag) >= 2 && tag[0] == "f" && tag[1] == platformARM64 {
			return nil
		}
	}
	return fmt.Errorf("platform must include %s", platformARM64)
}

func UnsupportedQuery(_ rely.Client, _ string, filters nostr.Filters) error {
	return store.Validate(filters...)
}

// NotAnchored returns an error if the event is not "anchored" to an existing event.
// Anchoring means simply that the event references an existing root event.
// A catalog-key app coordinate matches either listing slot for that app id.
// A coordinate aimed at the current developer slot must name that listing.
func NotAnchored(db store.T, catalogKey string) func(_ rely.Client, e *nostr.Event) error {
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

				found, err := addressFound(ctx, db, catalogKey, ref)
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

				found, err := addressFound(ctx, db, catalogKey, ref)
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

// addressFound reports whether an address tag points at a listing this relay keeps.
// A catalog-key app coordinate matches either slot for that app id, so a
// developer-only app still anchors comments and zaps. Any other coordinate,
// including the current developer slot and a kind 30267 stack, must name that event.
func addressFound(ctx context.Context, db store.T, catalogKey string, ref events.AddressableRef) (bool, error) {
	if ref.Kind == events.KindApp && ref.Pubkey == catalogKey {
		return db.Has(ctx, nostr.Filter{
			Kinds: []int{events.KindApp},
			Tags:  nostr.TagMap{"d": []string{ref.DTag}},
		})
	}
	return db.Has(ctx, ref.Filter())
}

// NotAdmitted runs the publisher gate for listing events. The catalog key,
// paid access, a pubkey that already published a kind 32267, or a pubkey
// above the web-of-trust threshold may publish.
// Other kinds pass. See the publisher policy.
func NotAdmitted(gate *policy.Gate) func(_ rely.Client, e *nostr.Event) error {
	return func(_ rely.Client, e *nostr.Event) error {
		if gate == nil {
			return nil
		}
		switch e.Kind {
		case events.KindApp, events.KindRelease, events.KindAsset:
		default:
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := gate.Admit(ctx, e.PubKey)
		if errors.Is(err, policy.ErrNotAdmitted) {
			return ErrPublisherNotAllowed
		}
		if err != nil {
			slog.Error("policy: gate failed", "err", err, "event", e.ID)
			return ErrInternal
		}
		return nil
	}
}

func NotAllowed(db store.T, pol *policy.Store, blobs Blossom) func(_ rely.Client, e *nostr.Event) error {
	return func(_ rely.Client, e *nostr.Event) error {
		if pol == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		verdict, err := pol.EventVerdict(ctx, e)
		if err != nil {
			slog.Error("policy: failed to check event", "err", err, "event", e.ID)
			return ErrInternal
		}
		if verdict.BanPublisher {
			// A banned file hash is decided from the stored bytes, not the tag alone.
			if x, ok := events.Find(e.Tags, "x"); ok {
				hash, hashErr := blossom.ParseHash(x)
				if hashErr != nil {
					return ErrFileNotAllowed
				}
				ready, readyErr := blobs.Has(ctx, hash)
				if readyErr != nil {
					slog.Error("policy: failed to check blob", "err", readyErr, "event", e.ID)
					return ErrInternal
				}
				if !ready {
					return nil
				}
			}
			for _, pub := range verdict.Publishers {
				if err := ApplyBan(ctx, db, pol, pub, verdict.Reason); err != nil {
					slog.Error("policy: failed to ban publisher", "err", err, "publisher", pub.ID)
					return ErrInternal
				}
			}
			return ErrFileNotAllowed
		}
		if verdict.Reject {
			pub, normErr := policy.Normalize(policy.KindNostr, e.PubKey)
			if normErr == nil {
				banned, banErr := pol.Blacklisted(ctx, pub)
				if banErr != nil {
					slog.Error("policy: failed to check publisher", "err", banErr, "event", e.ID)
					return ErrInternal
				}
				if banned {
					return ErrEventPubkeyBlocked
				}
			}
			return ErrPublisherNotAllowed
		}
		return nil
	}
}
