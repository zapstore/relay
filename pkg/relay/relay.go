// The relay package is responsible for setting up the relay.
// It exposes a [Setup] function to create a new relay with the given config.
package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip11"
	"github.com/nbd-wtf/go-nostr/nip13"
	"github.com/pippellia-btc/blossom"
	"github.com/pippellia-btc/rely/v2"
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/policy"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay/store"
	"github.com/zapstore/relay/pkg/search"
)

var (
	ErrEventKindNotAllowed = errors.New("event kind is not in the allowed list")
	ErrEventPubkeyBlocked  = errors.New("event pubkey is not allowed. Visit https://zapstore.dev/docs/publish for more information.")
	ErrPublisherNotAllowed = errors.New("publisher is not allowed. Visit https://zapstore.dev/docs/publish for more information.")
	ErrFileNotAllowed      = errors.New("file is not allowed. Visit https://zapstore.dev/docs/publish for more information.")

	ErrTooManyFilters  = errors.New("number of filters exceed the maximum allowed per REQ")
	ErrFiltersTooVague = errors.New("filters are too vague")

	ErrInternal    = errors.New("internal error, please contact the Zapstore team.")
	ErrRateLimited = errors.New("rate-limited: slow down chief")
	ErrReadOnly    = errors.New("relay is read-only")

	errNothingDeleted = errors.New("deletion matched nothing")
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
	pol       *policy.Store
	analytics *analytics.Engine

	blossom     Blossom
	uploads     chan upload
	profileJobs chan string
	apks        *apkReader
	process     Processor
	procs       *processQueue
	readAPK     func(io.ReadSeeker) (signing, error)
	fetch       *http.Client

	search *search.Engine
}

type upload struct {
	hash blossom.Hash
	mime string
}

// Blossom is the subset of blossom the relay uses.
// Has reports a blob in the buffer zone or served. Open returns its bytes.
// Promote serves a blob from the buffer zone. Expire deletes blobs still in
// the buffer zone before the cutoff.
type Blossom interface {
	Has(ctx context.Context, hash blossom.Hash) (bool, error)
	Open(ctx context.Context, hash blossom.Hash) (io.ReadCloser, error)
	Promote(ctx context.Context, hash blossom.Hash) error
	Expire(ctx context.Context, before time.Time) (int, error)
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
	readOnly bool,
) (*T, error) {

	server := rely.NewRelay(
		rely.WithAuthURL(config.Hostname),
		rely.WithInfo(nip11Doc(config.Info, readOnly)),
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
	if readOnly {
		server.Reject.Event.Append(readOnlyEvent)
	}
	server.Reject.Event.Append(
		RateEventIP(limiter),
		KindNotAllowed(config.AllowedKinds),
		rely.InvalidID,
		rely.InvalidSignature,
		InvalidStructure,
		NoZapTag,
		NotARM64,
		TooLittleWork,
		NotABugReport(config.Info.Pubkey),
		NotAnchored(store, config.Info.Pubkey),
		NotAllowed(pol),
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
		pol:       pol,
		analytics: analytics,

		blossom:     blssm,
		uploads:     make(chan upload, 100),
		profileJobs: make(chan string, 100),
		apks:        newAPKReader(),
		procs:       newProcessQueue(),
		readAPK:     readSigning,
		fetch:       publicFetch,
	}

	server.On.Event = relay.save
	server.On.Req = relay.query
	return relay, nil
}

func nip11Doc(info Info, readOnly bool) nip11.RelayInformationDocument {
	doc := info.NIP11()
	if readOnly {
		doc.Limitation = &nip11.RelayLimitationDocument{RestrictedWrites: true}
	}
	return doc
}

func readOnlyEvent(rely.Client, *nostr.Event) error {
	return ErrReadOnly
}

// Handler is the WebSocket relay. Mount it at "/" beside other routes.
func (r *T) Handler() http.Handler { return r.server }

// StartAndServe starts the relay. handler receives every request; nil serves
// only the WebSocket relay. Every app has its selected release before the
// relay listens.
func (r *T) StartAndServe(ctx context.Context, addr string, handler http.Handler) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := reselect(ctx, r.store, r.config.Info.Pubkey); err != nil {
		return fmt.Errorf("fill listings: %w", err)
	}
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		r.runReconcile(ctx)
	}()
	go func() {
		defer wg.Done()
		r.runProfileWorker(ctx)
	}()
	go func() {
		defer wg.Done()
		r.runAPKReader(ctx)
	}()
	go func() {
		defer wg.Done()
		r.runProcessor(ctx)
	}()
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
	var err error
	select {
	case <-ctx.Done():
		shut, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		err = srv.Shutdown(shut)
		r.server.Wait()
	case err = <-exit:
	}
	cancel()
	wg.Wait()
	return err
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

// sweepInterval is how often reconcile runs, after once at start. Promotion
// does not wait for it: an event, an upload, a read, or processing promotes
// what it completes. The sweep picks up what a restart left pending and
// deletes what expired.
const sweepInterval = time.Hour

func (r *T) runReconcile(ctx context.Context) {
	sweep := func() {
		if err := r.reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("reconcile failed", "error", err)
		}
	}
	sweep()
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			sweep()

		case u := <-r.uploads:
			if u.mime == "application/vnd.android.package-archive" {
				err := r.promoteWaitingOn(ctx, u.hash.Hex())
				if err != nil && !errors.Is(err, context.Canceled) {
					slog.Error("upload: promote failed", "blob", u.hash.Hex(), "error", err)
				}
			}
		}
	}
}

// reconcile promotes what is ready in the buffer zone: listings, and claims
// checked against a listed app's APK. Then it deletes the events and files
// that have waited longer than RemovePendingAfter.
func (r *T) reconcile(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	pending, err := r.store.ListPending(ctx, bufferedKinds())
	if err != nil {
		return fmt.Errorf("failed to reconcile events: %w", err)
	}

	seen := map[string]struct{}{}
	var errs []error
	for _, event := range pending {
		if event.Kind == events.KindIdentityProof {
			continue
		}
		appID, ok := listingAppID(&event)
		if !ok {
			continue
		}
		key := event.PubKey + ":" + appID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if _, err := r.promoteListing(ctx, event.PubKey, appID, ""); err != nil {
			errs = append(errs, fmt.Errorf("promote %s: %w", appID, err))
		}
	}
	for _, event := range pending {
		if event.Kind != events.KindIdentityProof {
			continue
		}
		if kept, err := pendingHas(ctx, r.store, event.ID); err != nil || !kept {
			continue
		}
		if _, err := r.promoteClaim(ctx, event, ""); err != nil {
			errs = append(errs, fmt.Errorf("promote proof %s: %w", event.ID, err))
		}
	}

	if err := reselect(ctx, r.store, r.config.Info.Pubkey); err != nil {
		errs = append(errs, err)
	}

	cutoff := time.Now().UTC().Add(-r.config.RemovePendingAfter)
	if err := r.store.DeleteExpiredPending(ctx, cutoff); err != nil {
		errs = append(errs, fmt.Errorf("failed to delete expired pending events: %w", err))
	}
	if n, err := r.blossom.Expire(ctx, cutoff); err != nil {
		errs = append(errs, fmt.Errorf("failed to delete expired files: %w", err))
	} else if n > 0 {
		slog.Info("buffer zone: expired files", "count", n)
	}
	return errors.Join(errs...)
}

// reselect fills the selection of apps whose listings changed. On failure
// the rows stay empty, and the next event or reconcile fills them.
func (r *T) reselect(ctx context.Context) {
	if err := reselect(ctx, r.store, r.config.Info.Pubkey); err != nil {
		slog.Error("relay: failed to reselect listings", "error", err)
	}
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

// changesListings reports whether persisting an event of this kind can store
// or delete a listing event: a deletion, a listing event, or a proof that
// promotes or replaces a developer slot. Only those make reselect do work.
func changesListings(kind int) bool {
	return kind == nostr.KindDeletion || listingKind(kind) || kind == events.KindIdentityProof
}

func (r *T) persist(c rely.Client, event *nostr.Event) persistResult {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if changesListings(event.Kind) {
		defer r.reselect(ctx)
	}

	if r.analytics != nil {
		r.analytics.RecordEvent()
	}

	switch {
	case event.Kind == nostr.KindDeletion:
		if err := r.handleDelete(ctx, event); err != nil {
			if errors.Is(err, errNothingDeleted) {
				return persistResult{msg: errNothingDeleted.Error()}
			}
			slog.Error("relay: failed to fullfil delete", "event", event.ID, "error", err)
			return persistResult{msg: ErrInternal.Error()}
		}

	case listingKind(event.Kind):
		return r.holdListing(ctx, event)

	case event.Kind == events.KindIdentityProof && !events.ProofRevoked(*event):
		return r.holdProof(ctx, event)

	case event.Kind == events.KindIdentityProof:
		if err := r.storeRevocation(ctx, event); err != nil {
			slog.Error("relay: failed to store revocation", "event", event.ID, "error", err)
			return persistResult{msg: ErrInternal.Error()}
		}

	case nostr.IsRegularKind(event.Kind):
		if _, err := r.store.Save(ctx, event); err != nil {
			slog.Error("relay: failed to save regular event", "event", event.ID, "error", err)
			return persistResult{msg: ErrInternal.Error()}
		}

	case nostr.IsReplaceableKind(event.Kind) || nostr.IsAddressableKind(event.Kind):
		if _, err := r.store.Replace(ctx, event); err != nil {
			slog.Error("relay: failed to replace event", "event", event.ID, "error", err)
			return persistResult{msg: ErrInternal.Error()}
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
		n, pendingErr := r.dropPendingRefs(ctx, event, "")
		if pendingErr != nil {
			return pendingErr
		}
		deleted += n
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
		n, pendingErr := r.dropPendingRefs(ctx, event, event.PubKey)
		if pendingErr != nil {
			return pendingErr
		}
		deleted += n
	}

	if deleted == 0 {
		return errNothingDeleted
	}

	// Save the delete request. Clients will fetch these to remove the deleted events from their local cache.
	if _, err := r.store.Save(ctx, event); err != nil {
		return fmt.Errorf("failed to save delete request: %w", err)
	}
	return nil
}

func cloneTags(tags nostr.TagMap) nostr.TagMap {
	out := make(nostr.TagMap, len(tags)+1)
	for key, values := range tags {
		out[key] = values
	}
	return out
}

// queryTimeout bounds one REQ, from the first SQL statement to the last.
const queryTimeout = 5 * time.Second

func (r *T) query(ctx context.Context, c rely.Client, id string, filters nostr.Filters) ([]nostr.Event, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var result []nostr.Event
	var err error
	if len(filters) == 1 {
		if hash, ok := store.FileHashSearch(filters[0]); ok {
			// The phone already has the file. Return the event that names its
			// hash, including one from a slot that is not selected.
			f := filters[0]
			f.Search = ""
			f.Tags = cloneTags(f.Tags)
			f.Tags["x"] = []string{hash}
			result, err = r.store.Query(ctx, f)
			if errors.Is(err, store.ErrUnsupportedREQ) {
				return nil, err
			}
			if err != nil {
				return nil, queryFailed(ctx, "failed to query events", err, filters, start)
			}
			if r.analytics != nil {
				r.analytics.RecordReq(filters)
			}
			return result, nil
		}
	}
	if assetOnly, live := liveRead(filters); live {
		budget := 0
		if c != nil && c.UID() != SocketUID {
			budget = c.RemainingCapacity()
		}
		result, err = r.liveREQ(ctx, filters[0], assetOnly, budget)
		if err != nil {
			return nil, queryFailed(ctx, "failed live read", err, filters, start)
		}
	} else if textSearch(filters) {
		result, err = r.searchREQ(ctx, filters[0])
		if errors.Is(err, store.ErrUnsupportedREQ) {
			return nil, err
		}
		if err != nil {
			return nil, queryFailed(ctx, "failed to search", err, filters, start)
		}
	} else {
		result, err = r.store.Query(ctx, filters...)
		if errors.Is(err, store.ErrUnsupportedREQ) {
			return nil, err
		}
		if err != nil {
			return nil, queryFailed(ctx, "failed to query events", err, filters, start)
		}
		result, err = selectSlots(ctx, r.store, filters, result)
		if err != nil {
			return nil, queryFailed(ctx, "failed to select listing slots", err, filters, start)
		}
	}

	if r.analytics != nil {
		r.analytics.RecordReq(filters)
	}
	return result, nil
}

// queryFailed logs a failed REQ and returns the error the client sees. A
// canceled ctx means the client closed the subscription or disconnected:
// rely sends nothing, so it is not logged. A REQ past queryTimeout is logged
// as slow with its filters. The client never sees the SQL error.
func queryFailed(ctx context.Context, msg string, err error, filters nostr.Filters, start time.Time) error {
	switch ctx.Err() {
	case context.Canceled:
		return ctx.Err()
	case context.DeadlineExceeded:
		slog.Warn("relay: slow query", "step", msg, "elapsed", time.Since(start), "filters", filters)
	default:
		slog.Error("relay: "+msg, "error", err, "filters", filters)
	}
	return ErrInternal
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
		if client.UID() == SocketUID {
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
		if client.UID() == SocketUID {
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

const devicePoW = 20

// TooLittleWork rejects a device-private event whose id has fewer than
// devicePoW leading zero bits: every kind 30078, and a kind 30267 with content.
func TooLittleWork(_ rely.Client, e *nostr.Event) error {
	switch {
	case e.Kind == events.KindAppSettings:
	case e.Kind == events.KindStack && e.Content != "":
	default:
		return nil
	}
	if nip13.Difficulty(e.ID) < devicePoW {
		return fmt.Errorf("pow: difficulty %d is required", devicePoW)
	}
	return nil
}

const (
	reportPoW      = 16
	maxReportBytes = 64 << 10
)

// NotABugReport accepts a kind 1059 only as an anonymous bug report: one p
// tag naming the catalog key, reportPoW leading zero bits on the id, and at
// most maxReportBytes of content. Other kinds pass.
func NotABugReport(catalogKey string) func(rely.Client, *nostr.Event) error {
	return func(_ rely.Client, e *nostr.Event) error {
		if e.Kind != events.KindGiftWrap {
			return nil
		}
		var recipients []string
		for _, tag := range e.Tags {
			if len(tag) >= 2 && tag[0] == "p" {
				recipients = append(recipients, tag[1])
			}
		}
		if len(recipients) != 1 || recipients[0] != catalogKey {
			return errors.New("kind 1059: must be addressed to the catalog key only")
		}
		if len(e.Content) > maxReportBytes {
			return fmt.Errorf("kind 1059: content is over %d bytes", maxReportBytes)
		}
		if nip13.Difficulty(e.ID) < reportPoW {
			return fmt.Errorf("pow: difficulty %d is required", reportPoW)
		}
		return nil
	}
}

func UnsupportedQuery(_ rely.Client, _ string, filters nostr.Filters) error {
	return store.Validate(filters...)
}

// NoZapTag rejects a kind 32267 that carries a zap tag. The payee is the
// active C1 proof pubkey, not a tag on the listing.
func NoZapTag(_ rely.Client, e *nostr.Event) error {
	if e.Kind != events.KindApp {
		return nil
	}
	for _, tag := range e.Tags {
		if len(tag) > 0 && tag[0] == "zap" {
			return errors.New("kind 32267: zap tag is not allowed")
		}
	}
	return nil
}

// NotAnchored returns an error if the event is not "anchored" to an existing event.
// Anchoring means simply that the event references an existing root event.
// An app coordinate is accepted for any pubkey when a listing for that app
// exists. A kind 9735 p tag must be a known pubkey for the app.
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
			// A reply names a stored kind 11 or 1111 with its e tag.
			return anchored(ctx, db, e, "A", true, &nostr.Filter{Kinds: []int{events.KindForumPost, events.KindComment}})
		case events.KindZap:
			if err := anchored(ctx, db, e, "a", true, &nostr.Filter{}); err != nil {
				return err
			}
			return zapPaysKnownPubkey(ctx, db, catalogKey, e)
		case events.KindReport:
			return anchored(ctx, db, e, "a", false, nil)
		}
		return nil
	}
}

// anchored checks a comment, zap, or report. The address in addrTag must be an
// app coordinate this relay accepts, or a stored stack when stacks is set. On
// kinds 1111 and 1984 an app coordinate needs an i tag with that app id; a
// zap receipt is written by a Lightning server and its i is ignored. When
// reply is set, an e tag naming a stored event matching reply may stand in
// for the address.
func anchored(ctx context.Context, db store.T, e *nostr.Event, addrTag string, stacks bool, reply *nostr.Filter) error {
	addr, hasAddr := events.Find(e.Tags, addrTag)
	var eTag string
	var hasE bool
	if reply != nil {
		eTag, hasE = events.Find(e.Tags, "e")
	}
	if !hasAddr && !hasE {
		if reply == nil {
			return fmt.Errorf("kind %d: must have an '%s' tag", e.Kind, addrTag)
		}
		return fmt.Errorf("kind %d: must have an '%s' tag or 'e' tag", e.Kind, addrTag)
	}

	if hasAddr {
		ref, err := events.ParseAddressableRef(addr)
		if err == nil {
			err = ref.Validate()
		}
		if err == nil && ref.Kind != events.KindApp && !(stacks && ref.Kind == events.KindStack) {
			err = fmt.Errorf("kind %d", ref.Kind)
		}
		if err != nil {
			return fmt.Errorf("kind %d: '%s' tag must reference %s: %w", e.Kind, addrTag, anchorKinds(stacks), err)
		}
		if ref.Kind == events.KindApp && e.Kind != events.KindZap {
			if appID, _ := events.Find(e.Tags, "i"); appID != ref.DTag {
				return fmt.Errorf("kind %d: 'i' tag must be the app id %s", e.Kind, ref.DTag)
			}
		}
		found, err := addressFound(ctx, db, ref)
		if err != nil {
			slog.Error("NotAnchored: failed to check address", "error", err, "event", e.ID, "tag", addr)
			return ErrInternal
		}
		if !found {
			return fmt.Errorf("kind %d: '%s' tag reference not found on this relay", e.Kind, addrTag)
		}
	}

	if hasE {
		filter := *reply
		filter.IDs = []string{eTag}
		found, err := db.Has(ctx, filter)
		if err != nil {
			slog.Error("NotAnchored: failed to check reference", "error", err, "event", e.ID, "tag", eTag)
			return ErrInternal
		}
		if !found {
			return fmt.Errorf("kind %d: 'e' tag reference not found on this relay", e.Kind)
		}
	}
	return nil
}

func anchorKinds(stacks bool) string {
	if stacks {
		return "a kind 32267 or kind 30267"
	}
	return "a kind 32267"
}

// addressFound reports whether an address tag points at a listing this relay keeps.
// A kind 32267 coordinate matches when a listing for that app id exists,
// whatever its pubkey: the catalog key, the earlier catalog key, a developer,
// or another catalog. Any other coordinate, including a kind 30267 stack,
// must name that event.
func addressFound(ctx context.Context, db store.T, ref events.AddressableRef) (bool, error) {
	if ref.Kind != events.KindApp {
		return db.Has(ctx, ref.Filter())
	}
	return db.Has(ctx, nostr.Filter{
		Kinds: []int{events.KindApp},
		Tags:  nostr.TagMap{"d": []string{ref.DTag}},
	})
}

// zapPaysKnownPubkey rejects a kind 9735 on an app unless its p tag is a
// known pubkey for the app: the catalog key, the earlier catalog key, a
// listing signer, or the active C1 owner. A stack zap is not checked.
func zapPaysKnownPubkey(ctx context.Context, db store.T, catalogKey string, e *nostr.Event) error {
	addr, ok := events.Find(e.Tags, "a")
	if !ok {
		return nil
	}
	ref, err := events.ParseAddressableRef(addr)
	if err != nil || ref.Kind != events.KindApp {
		return nil
	}
	recipient, hasP := events.Find(e.Tags, "p")
	if !hasP {
		return fmt.Errorf("kind %d: must have a 'p' tag", e.Kind)
	}
	known, err := knownPubkey(ctx, db, catalogKey, ref.DTag, strings.ToLower(recipient))
	if err != nil {
		return err
	}
	if !known {
		return fmt.Errorf("kind %d: 'p' tag must be a known pubkey for %s", e.Kind, ref.DTag)
	}
	return nil
}

func knownPubkey(ctx context.Context, db store.T, catalogKey, appID, pubkey string) (bool, error) {
	if pubkey == catalogKey || pubkey == events.EarlierCatalogKey {
		return true, nil
	}
	signed, err := db.Has(ctx, nostr.Filter{
		Kinds:   []int{events.KindApp},
		Authors: []string{pubkey},
		Tags:    nostr.TagMap{"d": []string{appID}},
	})
	if err != nil {
		slog.Error("NotAnchored: failed to check listing signer", "error", err, "app", appID)
		return false, ErrInternal
	}
	if signed {
		return true, nil
	}
	owner, found, err := proofOwner(ctx, db, catalogKey, appID)
	if err != nil {
		return false, err
	}
	return found && strings.EqualFold(pubkey, owner), nil
}

func proofOwner(ctx context.Context, db store.T, catalogKey, appID string) (string, bool, error) {
	cert, ok, err := appCertificate(ctx, db, catalogKey, appID)
	if err != nil || !ok {
		return "", false, err
	}
	owner, _, ok, err := activeDeveloper(ctx, db, []string{cert}, nil)
	if err != nil || !ok {
		return "", false, err
	}
	return owner, true, nil
}

// NotAdmitted runs the publisher gate for listing events. The catalog key,
// a pubkey that already published a kind 32267, a whitelisted pubkey, or a
// pubkey above the web-of-trust threshold may publish. An app id that
// already has a kind 32267 skips the gate; the C1 check still decides the slot.
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
		appID, _ := listingAppID(e)
		err := gate.AdmitListing(ctx, e.PubKey, appID)
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

func NotAllowed(pol *policy.Store) func(_ rely.Client, e *nostr.Event) error {
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
		// A banned file hash rejects the event. It does not blacklist the signer.
		if verdict.Reason != "" {
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
