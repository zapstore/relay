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
	"github.com/pippellia-btc/rely"
	sqlite "github.com/vertex-lab/nostr-sqlite"
	"github.com/zapstore/relay/pkg/acl"
	"github.com/zapstore/relay/pkg/analytics"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/indexing"
	"github.com/zapstore/relay/pkg/rate"
	"github.com/zapstore/relay/pkg/relay/linkverify"
	"github.com/zapstore/relay/pkg/relay/store"
	"github.com/zapstore/relay/pkg/search"
)

var (
	ErrEventKindNotAllowed = errors.New("event kind is not in the allowed list")
	ErrEventPubkeyBlocked  = errors.New("event pubkey is not allowed. Visit https://zapstore.dev/docs/publish for more information.")

	ErrAppAlreadyExists = errors.New(`failed to publish app: another pubkey has already published an app with the same 'd' tag identifier.
		This is a precautionary measure because Android doesn't allow apps with the same identifier to be installed side by side.
		Please use a different identifier or contact the Zapstore team for more information.`)

	ErrProfileUnknownPublisher = errors.New("kind 0 profile rejected: pubkey has no events on this relay")

	ErrTooManyFilters  = errors.New("number of filters exceed the maximum allowed per REQ")
	ErrFiltersTooVague = errors.New("filters are too vague")

	ErrInternal    = errors.New("internal error, please contact the Zapstore team.")
	ErrRateLimited = errors.New("rate-limited: slow down chief")
)

// indexerPubkeyFallback is the hardcoded zapstore indexer pubkey used when RELAY_PUBKEY is not set.
const indexerPubkeyFallback = "78ce6faa72264387284e647ba6938995735ec8c7d5c5a65737e55130f026307d"


func Setup(
	config Config,
	limiter rate.Limiter,
	acl *acl.Controller,
	store *sqlite.Store,
	analytics *analytics.Engine,
	c1 *linkverify.Verifier,
	indexingEngine *indexing.Engine, // nil = no demand-driven indexing
	searchEngine *search.Engine,     // nil = FTS5 only
) (*rely.Relay, error) {

	relay := rely.NewRelay(
		rely.WithAuthURL(config.Hostname),
		rely.WithInfo(config.Info.NIP11()),
		rely.WithQueueCapacity(config.QueueCapacity),
		rely.WithMaxMessageSize(config.MaxMessageBytes),
		rely.WithClientResponseLimit(config.ResponseLimit),
	)

	relay.Reject.Connection.Clear()
	relay.Reject.Connection.Append(
		RateConnectionIP(limiter),
		rely.RegistrationFailWithin(3*time.Second),
	)

	relay.Reject.Event.Clear()
	relay.Reject.Event.Append(
		RateEventIP(limiter),
		KindNotAllowed(config.AllowedKinds),
		rely.InvalidID,
		rely.InvalidSignature,
		InvalidStructure(),
		CommentScopedToKnownEvent(store),
		AuthorNotAllowed(acl, config.Info.Pubkey),
		ProfileKnownPublisher(store, config.Info.Pubkey),
		AppOwnership(store, config.Info.Pubkey),
	)

	relay.Reject.Req.Clear()
	relay.Reject.Req.Append(
		RateReqIP(limiter),
		FiltersExceed(config.MaxReqFilters),
		VagueFilters(3),
	)

	relay.On.Event = Save(store, analytics, c1, indexingEngine, searchEngine, config.Info.Pubkey)
	relay.On.Req = Query(store, analytics, indexingEngine, config.MaxFilterLimit)
	return relay, nil
}

func Save(db *sqlite.Store, analytics *analytics.Engine, c1 *linkverify.Verifier, idx *indexing.Engine, se *search.Engine, operatorPubkey string) func(c rely.Client, event *nostr.Event) error {
	return func(c rely.Client, event *nostr.Event) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		switch {
		case event.Kind == nostr.KindDeletion:
			if event.PubKey == operatorPubkey {
				// Operator-level deletion: bypass NIP-09 author check and delete any targeted events directly.
				for _, tag := range event.Tags {
					if len(tag) >= 2 && tag[0] == "e" {
						if err := deleteEvent(ctx, db, tag[1]); err != nil {
							slog.Error("relay: operator deletion failed", "error", err, "target_event", tag[1])
						}
						if se != nil {
							se.Delete(tag[1])
						}
					}
				}
			} else {
				if _, err := db.DeleteRequest(ctx, event); err != nil {
					slog.Error("relay: failed to fulfill the delete request", "error", err, "event", event.ID)
					return err
				}
			}

			if _, err := db.Save(ctx, event); err != nil {
				slog.Error("relay: failed to save the delete request", "error", err, "event", event.ID)
				return err
			}

		case nostr.IsRegularKind(event.Kind):
			if _, err := db.Save(ctx, event); err != nil {
				slog.Error("relay: failed to save the event", "error", err, "event", event.ID)
				return err
			}

		case nostr.IsReplaceableKind(event.Kind) || nostr.IsAddressableKind(event.Kind):
			if _, err := db.Replace(ctx, event); err != nil {
				slog.Error("relay: failed to replace the event", "error", err, "event", event.ID)
				return err
			}
			if se != nil && event.Kind == events.KindApp {
				se.Index(event)
			}
		}

		analytics.RecordEvent(c, event)

		// C1 verification runs asynchronously after save
		if c1 != nil {
			c1.OnEvent(ctx, event)
		}

		return nil
	}
}

func Query(db *sqlite.Store, analytics *analytics.Engine, idx *indexing.Engine, maxFilterLimit int) func(ctx context.Context, c rely.Client, id string, filters nostr.Filters) ([]nostr.Event, error) {
	return func(ctx context.Context, client rely.Client, id string, filters nostr.Filters) ([]nostr.Event, error) {
		for i := range filters {
			if filters[i].Limit > maxFilterLimit {
				filters[i].Limit = maxFilterLimit
			}
		}

		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		queryFilters, expanded := withLegacyHTagFallback(filters)
		result, err := db.Query(ctx, queryFilters...)
		if errors.Is(err, store.ErrUnsupportedREQ) {
			return nil, err
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("relay: failed to query events", "error", err, "filters", filters)
			return nil, err
		}

		if expanded {
			result = deduplicateEvents(result)
		}

		analytics.RecordReq(client, id, filters, result)

		if idx != nil {
			recordDemandSignals(idx, id, filters, result)
		}

		return result, nil
	}
}

// recordDemandSignals records discovery misses and release requests non-blocking.
// Release-request signals are gated to known Zapstore client subscription prefixes,
// filtering out bots and non-Zapstore clients.
func recordDemandSignals(idx *indexing.Engine, subID string, filters nostr.Filters, result []nostr.Event) {
	wantsReleases := strings.HasPrefix(subID, "app-updates") ||
		strings.HasPrefix(subID, "app-detail") ||
		strings.HasPrefix(subID, "web-app-detail")

	for _, filter := range filters {
		// Discovery miss: NIP-50 search on kind 32267 with a GitHub URL and zero results.
		// Not gated on subID — discovery misses are always meaningful.
		if filter.Search != "" && len(result) == 0 {
			if isKindOnly(filter.Kinds, events.KindApp) {
				idx.RecordDiscoveryMiss(filter.Search)
			}
		}

		if !wantsReleases {
			continue
		}

		if hasReleaseKind(filter.Kinds) && len(result) > 0 {
			if iVals, ok := filter.Tags["i"]; ok {
				for _, appID := range iVals {
					idx.RecordReleaseRequest(appID)
				}
			} else {
				// Extract app IDs from returned events
				for _, ev := range result {
					if appID, ok := events.Find(ev.Tags, "i"); ok {
						idx.RecordReleaseRequest(appID)
					}
				}
			}
		}
	}
}

func isKindOnly(kinds []int, kind int) bool {
	return len(kinds) == 1 && kinds[0] == kind
}

func hasReleaseKind(kinds []int) bool {
	for _, k := range kinds {
		if k == events.KindRelease || k == events.KindAsset {
			return true
		}
	}
	return false
}

func RateConnectionIP(limiter rate.Limiter) func(_ rely.Stats, request *http.Request) error {
	return func(_ rely.Stats, request *http.Request) error {
		cost := 1.0
		if !limiter.Allow(rely.GetIP(request).Group(), cost) {
			return ErrRateLimited
		}
		return nil
	}
}

func RateEventIP(limiter rate.Limiter) func(client rely.Client, _ *nostr.Event) error {
	return func(client rely.Client, _ *nostr.Event) error {
		cost := 5.0
		if !limiter.Allow(client.IP().Group(), cost) {
			client.Disconnect()
			return ErrRateLimited
		}
		return nil
	}
}

func RateReqIP(limiter rate.Limiter) func(client rely.Client, id string, filters nostr.Filters) error {
	return func(client rely.Client, id string, filters nostr.Filters) error {
		cost := 1.0
		if len(filters) > 10 {
			cost = 5.0
		}

		if !limiter.Allow(client.IP().Group(), cost) {
			client.Disconnect()
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
		points += 3
	}
	if filter.Limit != 0 && filter.Limit < 100 {
		points += 1
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

func InvalidStructure() func(_ rely.Client, e *nostr.Event) error {
	return func(_ rely.Client, e *nostr.Event) error {
		return events.Validate(e)
	}
}

// AppOwnership enforces one publisher per app ID (kind 32267 d-tag) with role-aware transitions.
//
// Transition table:
//   - same pubkey → same pubkey:         accept (NIP-33 replace)
//   - indexer → non-indexer:             delete old 32267, accept (indexer takeover)
//   - non-indexer → indexer:             delete indexer's 32267, accept (developer reclaim)
//   - anyone else → anyone else:         reject
func AppOwnership(db *sqlite.Store, indexerPubkey string) func(_ rely.Client, e *nostr.Event) error {
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
			// No conflict — new app or NIP-33 replace by same pubkey
			return nil
		}

		newIsIndexer := e.PubKey == indexerPubkey
		existingIsIndexer := existingPubkey == indexerPubkey

		switch {
		case newIsIndexer && !existingIsIndexer:
			// Indexer takeover: delete developer's old 32267
			if err := deleteEvent(ctx, db, existingID); err != nil {
				slog.Error("AppOwnership: failed to delete old app event for indexer takeover", "event_id", existingID, "error", err)
				return ErrInternal
			}
			slog.Info("AppOwnership: indexer takeover", "app_id", appID, "old_pubkey", existingPubkey[:16])
			return nil

		case !newIsIndexer && existingIsIndexer:
			// Developer reclaim: delete indexer's 32267
			if err := deleteEvent(ctx, db, existingID); err != nil {
				slog.Error("AppOwnership: failed to delete indexer app event for developer reclaim", "event_id", existingID, "error", err)
				return ErrInternal
			}
			slog.Info("AppOwnership: developer reclaim", "app_id", appID, "new_pubkey", e.PubKey[:16])
			return nil

		default:
			// Non-indexer vs non-indexer: reject
			return ErrAppAlreadyExists
		}
	}
}

// deleteEvent removes an event and its tags from the relay store.
// This is a relay-operator-level store management operation, not a Nostr protocol deletion.
func deleteEvent(ctx context.Context, db *sqlite.Store, eventID string) error {
	_, err := db.DB.ExecContext(ctx, `DELETE FROM tags WHERE event_id = ?`, eventID)
	if err != nil {
		return fmt.Errorf("delete tags: %w", err)
	}
	_, err = db.DB.ExecContext(ctx, `DELETE FROM events WHERE id = ?`, eventID)
	if err != nil {
		return fmt.Errorf("delete event: %w", err)
	}
	return nil
}

// CommentScopedToKnownEvent rejects kind 1111 comments that are not anchored to a known event
// in the relay store. Two cases are accepted:
//
//   - Root comment: has an uppercase "A" tag of the form "32267:<pubkey>:<d-tag>" or
//     "30267:<pubkey>:<d-tag>" referencing an app event that exists in the store.
//   - Reply: has an "e" tag referencing the event ID of a kind 1111 comment that exists
//     in the store.
//
// Any other kind 1111 is rejected.
func CommentScopedToKnownEvent(db *sqlite.Store) func(_ rely.Client, e *nostr.Event) error {
	return func(_ rely.Client, e *nostr.Event) error {
		if e.Kind != events.KindComment {
			return nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		// Root comment: uppercase A tag must reference a known 32267/30267 app event.
		if rootAddr, ok := events.Find(e.Tags, "A"); ok {
			parts := strings.SplitN(rootAddr, ":", 3)
			if len(parts) != 3 || (parts[0] != "32267" && parts[0] != "30267") {
				return errors.New("kind 1111: 'A' tag must reference a kind 32267 or 30267 app event")
			}
			kind, pubkey, dTag := parts[0], parts[1], parts[2]
			var exists int
			err := db.DB.QueryRowContext(ctx,
				`SELECT 1 FROM events AS e JOIN tags AS t ON t.event_id = e.id
				 WHERE e.kind = ? AND e.pubkey = ? AND t.key = 'd' AND t.value = ? LIMIT 1`,
				kind, pubkey, dTag,
			).Scan(&exists)
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("kind 1111: referenced app event not found on this relay")
			}
			if err != nil {
				slog.Error("CommentScopedToKnownEvent: failed to query app event", "error", err)
				return ErrInternal
			}
			return nil
		}

		// Reply: e tag must reference a known kind 1111 comment by event ID.
		if parentID, ok := events.Find(e.Tags, "e"); ok {
			var exists int
			err := db.DB.QueryRowContext(ctx,
				`SELECT 1 FROM events WHERE kind = 1111 AND id = ? LIMIT 1`,
				parentID,
			).Scan(&exists)
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("kind 1111 reply: referenced parent comment not found on this relay")
			}
			if err != nil {
				slog.Error("CommentScopedToKnownEvent: failed to query parent comment", "error", err)
				return ErrInternal
			}
			return nil
		}

		return errors.New("kind 1111: must have an 'A' tag (root scope) or 'e' tag (reply to kind 1111)")
	}
}

// ProfileKnownPublisher rejects kind 0 profile events whose pubkey has no other events
// on this relay. This prevents arbitrary profile spam while allowing publishers who
// already have content here to maintain their profile metadata.
func ProfileKnownPublisher(db *sqlite.Store, operatorPubkey string) func(_ rely.Client, e *nostr.Event) error {
	return func(_ rely.Client, e *nostr.Event) error {
		if e.Kind != events.KindProfile {
			return nil
		}
		if operatorPubkey != "" && e.PubKey == operatorPubkey {
			return nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var exists int
		err := db.DB.QueryRowContext(ctx,
			`SELECT 1 FROM events WHERE pubkey = ? AND kind != 0 LIMIT 1`,
			e.PubKey,
		).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrProfileUnknownPublisher
		}
		if err != nil {
			slog.Error("ProfileKnownPublisher: failed to query events", "error", err)
			return ErrInternal
		}
		return nil
	}
}

func AuthorNotAllowed(acl *acl.Controller, operatorPubkey string) func(_ rely.Client, e *nostr.Event) error {
	return func(_ rely.Client, e *nostr.Event) error {
		// Operator pubkey is always allowed — no ACL check needed.
		if operatorPubkey != "" && e.PubKey == operatorPubkey {
			return nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Open kinds (0, 1111, 9735, 30267, 30509) skip the allow-list and unknown-pubkey policy,
		// but blocked pubkeys are still rejected. Kind 0 is additionally gated by ProfileKnownPublisher.
		if e.Kind == events.KindProfile || e.Kind == events.KindComment || e.Kind == events.KindZap || e.Kind == events.KindAppSet || e.Kind == events.KindIdentityProof {
			blocked, err := acl.IsBlocked(ctx, e.PubKey)
			if err != nil {
				slog.Error("relay: failed to check if pubkey is blocked", "error", err)
				return ErrEventPubkeyBlocked
			}
			if blocked {
				return ErrEventPubkeyBlocked
			}
			return nil
		}

		allow, err := acl.AllowEvent(ctx, e)
		if err != nil {
			// fail closed policy
			slog.Error("relay: failed to check if pubkey is allowed", "error", err)
			return ErrEventPubkeyBlocked
		}
		if !allow {
			return ErrEventPubkeyBlocked
		}
		return nil
	}
}
