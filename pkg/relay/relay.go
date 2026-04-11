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

	ErrProfileUnknown = errors.New("kind 0 profile rejected: pubkey has no events on this relay")

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

// RestrictedKinds are the ones that must pass the full ACL verification.
var RestrictedKinds = []int{
	events.KindApp,
	events.KindRelease,
	events.KindAsset,
	events.KindCommunityCreation,
	events.KindIdentityProof,
}

// indexerPubkeyFallback is the hardcoded zapstore indexer pubkey used when RELAY_PUBKEY is not set.
const indexerPubkeyFallback = "78ce6faa72264387284e647ba6938995735ec8c7d5c5a65737e55130f026307d"

func Setup(
	config Config,
	limiter rate.Limiter,
	acl *acl.Controller,
	store *sqlite.Store,
	analytics *analytics.Engine,
	indexingEngine *indexing.Engine, // nil = no demand-driven indexing
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
		NotAnchored(store),
		AuthorNotAllowed(acl, config.Info.Pubkey),
		AppOwnership(store, config.Info.Pubkey),
	)

	relay.Reject.Req.Clear()
	relay.Reject.Req.Append(
		RateReqIP(limiter),
		FiltersExceed(config.MaxReqFilters),
		VagueFilters(3),
	)

	relay.On.Event = Save(store, analytics, indexingEngine, config.Info.Pubkey)
	relay.On.Req = Query(store, analytics, indexingEngine)
	return relay, nil
}

func Save(db *sqlite.Store, analytics *analytics.Engine, idx *indexing.Engine, operatorPubkey string) func(c rely.Client, event *nostr.Event) error {
	return func(c rely.Client, event *nostr.Event) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		switch {
		case event.Kind == nostr.KindDeletion:
			// Try regular NIP-09 deletion first (handles both e and a tags, but only for same pubkey)
			deleted, err := db.DeleteRequest(ctx, event)
			if err != nil {
				slog.Error("relay: failed to fulfill the delete request", "error", err, "event", event.ID)
				return err
			}

			// If nothing was deleted and this is the operator, fall back to operator-level deletion
			// (bypasses NIP-09 author check and can delete any event)
			if deleted == 0 && event.PubKey == operatorPubkey {
				for _, tag := range event.Tags {
					if len(tag) < 2 {
						continue
					}

					switch tag[0] {
					case "e":
						// Delete by event ID
						if err := deleteEvent(ctx, db, tag[1]); err != nil {
							slog.Error("relay: operator deletion failed", "error", err, "target_event", tag[1])
						} else {
							deleted++
						}

					case "a":
						// Delete by addressable coordinate (kind:pubkey:d-tag)
						ref, err := events.ParseAddressableRef(tag[1])
						if err != nil {
							slog.Warn("relay: invalid addressable ref in operator deletion", "ref", tag[1], "error", err)
							continue
						}
						if err := deleteEventByAddress(ctx, db, ref); err != nil {
							slog.Error("relay: operator deletion by address failed", "error", err, "ref", tag[1])
						} else {
							deleted++
						}
					}
				}

				if deleted > 0 {
					slog.Info("relay: operator deletion completed", "deleted_count", deleted, "event", event.ID)
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
		}

		analytics.RecordEvent(c, event)
		return nil
	}
}

func Query(db *sqlite.Store, analytics *analytics.Engine, idx *indexing.Engine) func(ctx context.Context, c rely.Client, id string, filters nostr.Filters) ([]nostr.Event, error) {
	return func(ctx context.Context, client rely.Client, id string, filters nostr.Filters) ([]nostr.Event, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		result, err := db.Query(ctx, filters...)
		if errors.Is(err, store.ErrUnsupportedREQ) {
			return nil, err
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("relay: failed to query events", "error", err, "filters", filters)
			return nil, err
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
				slog.Debug("indexing: release request skipped (subID prefix mismatch)", "sub_id", subID)
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

func isKindOnly(kinds []int, kind int) bool {
	return len(kinds) == 1 && kinds[0] == kind
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

// deleteEventByAddress removes an addressable event and its tags from the relay store.
// This is a relay-operator-level store management operation, not a Nostr protocol deletion.
func deleteEventByAddress(ctx context.Context, db *sqlite.Store, ref events.AddressableRef) error {
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("invalid addressable ref: %w", err)
	}

	// Find the event ID for this addressable coordinate
	var eventID string
	err := db.DB.QueryRowContext(ctx,
		`SELECT e.id FROM events AS e JOIN tags AS t ON t.event_id = e.id
		WHERE e.kind = ? AND e.pubkey = ? AND t.key = 'd' AND t.value = ? LIMIT 1`,
		ref.Kind, ref.Pubkey, ref.DTag,
	).Scan(&eventID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // Event doesn't exist, nothing to delete
		}
		return fmt.Errorf("query addressable event: %w", err)
	}

	// Delete the event
	return deleteEvent(ctx, db, eventID)
}

// NotAnchored returns an error if the event is not "anchored" to an existing event.
// Anchoring means simply that the event references an existing root event.
// Check [events.IsRoot].
func NotAnchored(db *sqlite.Store) func(_ rely.Client, e *nostr.Event) error {
	return func(_ rely.Client, e *nostr.Event) error {
		if slices.Contains(RootKinds, e.Kind) {
			// root events do not need to be "anchored" to an existing event
			return nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		switch e.Kind {
		case events.KindProfile:
			publisher, err := PubkeyExists(ctx, db, e.PubKey)
			if err != nil {
				slog.Error("NotAnchored: failed to check kind:0", "err", err)
				return ErrInternal
			}

			if !publisher {
				return ErrProfileUnknown
			}

		case events.KindComment, events.KindZap:
			aTag, hasA := events.Find(e.Tags, "A")
			eTag, hasE := events.Find(e.Tags, "e")
			if !hasA && !hasE {
				return errors.New("kind 1111: must have an 'A' tag (root) or 'e' tag (reply)")
			}

			if hasA {
				// A tag must reference a known app kind:32267 or stack kind:30267 event
				ref, err := events.ParseAddressableRef(aTag)
				if err != nil {
					return fmt.Errorf("kind 1111: 'A' tag must reference a kind 32267 or kind 30267: %w", err)
				}
				if ref.Kind != events.KindApp && ref.Kind != events.KindStack {
					return fmt.Errorf("kind 1111: 'A' tag must reference a kind 32267 or kind 30267: %d", ref.Kind)
				}

				exists, err := AddressExists(ctx, db, ref)
				if err != nil {
					slog.Error("NotAnchored: failed to check kind:1111", "error", err, "tag", aTag)
					return ErrInternal
				}

				if !exists {
					return errors.New("kind 1111: A tag reference not found on this relay")
				}
			}

			if hasE {
				// e tag must reference a known kind:11 or kind:1111 event
				exists, err := PostOrCommentExists(ctx, db, eTag)
				if err != nil {
					slog.Error("NotAnchored: failed to check kind:1111", "error", err, "tag", eTag)
					return ErrInternal
				}

				if !exists {
					return errors.New("kind 1111: e tag reference not found for kind:11 or kind:1111 on this relay")
				}
			}
		}
		return nil
	}
}

// PubkeyExists checks if a pubkey has published an event before.
func PubkeyExists(ctx context.Context, db *sqlite.Store, pubkey string) (bool, error) {
	var exists bool
	err := db.DB.QueryRowContext(ctx, `SELECT 1 FROM events WHERE pubkey = ? LIMIT 1`, pubkey).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return exists, nil
}

// AddressExists checks if an addressable reference (kind, pubkey, d-tag) exists in the store.
func AddressExists(ctx context.Context, db *sqlite.Store, ref events.AddressableRef) (bool, error) {
	if err := ref.Validate(); err != nil {
		return false, err
	}

	var exists bool
	err := db.DB.QueryRowContext(ctx,
		`SELECT 1 FROM events AS e JOIN tags AS t ON t.event_id = e.id
	 WHERE e.kind = ? AND e.pubkey = ? AND t.key = 'd' AND t.value = ? LIMIT 1`,
		ref.Kind, ref.Pubkey, ref.DTag,
	).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return exists, nil
}

// PostOrCommentExists checks if a post or comment exists in the store by event ID.
func PostOrCommentExists(ctx context.Context, db *sqlite.Store, id string) (bool, error) {
	if err := events.ValidateHash(id); err != nil {
		return false, err
	}

	var exists bool
	err := db.DB.QueryRowContext(ctx, `SELECT 1 FROM events WHERE id = ? AND kind IN (11, 1111) LIMIT 1`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return exists, nil
}

func AuthorNotAllowed(acl *acl.Controller, operatorPubkey string) func(_ rely.Client, e *nostr.Event) error {
	return func(_ rely.Client, e *nostr.Event) error {
		// Operator pubkey is always allowed — no ACL check needed.
		if operatorPubkey != "" && e.PubKey == operatorPubkey {
			return nil
		}

		if slices.Contains(RestrictedKinds, e.Kind) {
			// full ACL check for restrictive kinds
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			allow, err := acl.AllowEvent(ctx, e)
			if err != nil {
				// fail closed policy
				slog.Error("relay: failed to check if pubkey is allowed", "error", err)
				return ErrEventPubkeyBlocked
			}
			if !allow {
				return ErrEventPubkeyBlocked
			}
		}

		// other kinds just need not to be in the blocked pubkeys
		blocked, err := acl.IsBlocked(e.PubKey)
		if err != nil {
			slog.Error("relay: failed to check if pubkey is blocked", "error", err)
			return ErrEventPubkeyBlocked
		}
		if blocked {
			return ErrEventPubkeyBlocked
		}
		return nil
	}
}
