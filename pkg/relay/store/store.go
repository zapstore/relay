// The store package is responsible for storing nostr events in a sqlite database.
// It exposes a [New] function to create a new sqlite store with the given config.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	sqlite "github.com/vertex-lab/nostr-sqlite"
	"github.com/zapstore/relay/pkg/dbcheck"
	"github.com/zapstore/relay/pkg/events"
)

var ErrUnsupportedREQ = errors.New("unsupported REQ")

//go:embed schema.sql
var schema string

type T struct {
	*sqlite.Store
}

const (
	busyTimeout = 10 * time.Second
	cacheSize   = 128 * sqlite.MiB // per connection
	maxConns    = 16
)

// dsn returns the connection string for path. A PRAGMA run through the pool
// reaches one connection only, so per-connection settings go here, where
// the driver applies them to every connection it opens.
func dsn(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	if !strings.HasPrefix(path, "file:") {
		path = "file:" + path
	}
	return fmt.Sprintf("%s%s_busy_timeout=%d&_foreign_keys=1&_cache_size=%d",
		path, sep, busyTimeout.Milliseconds(), -int64(cacheSize/sqlite.KiB))
}

func New(path string) (T, error) {
	if err := requireMigrated(path); err != nil {
		return T{}, err
	}
	store, err := sqlite.New(
		dsn(path),
		sqlite.WithAdditionalSchema(schema),
		sqlite.WithQueryBuilder(queryBuilder),
		// sqlite.New overwrites the busy timeout and cache size on the
		// connection it uses for setup. These restore them there.
		sqlite.WithBusyTimeout(busyTimeout),
		sqlite.WithCacheSize(cacheSize),
		sqlite.WithoutEventPolicy(), // events have been validated by the relay
		sqlite.WithoutQueryPolicy(), // req queries have been validated by the relay
		sqlite.WithoutCountPolicy(), // count queries have been validated by the relay
	)
	if err != nil {
		return T{}, err
	}
	// Idle connections stay open so their page cache survives between queries.
	store.DB.SetMaxOpenConns(maxConns)
	store.DB.SetMaxIdleConns(maxConns)
	return T{Store: store}, nil
}

// requireMigrated refuses a database that predates pending_events.app_id. It
// runs before the schema, whose index on that column would fail on the old table.
func requireMigrated(path string) error {
	db, err := sql.Open("sqlite3", dsn(path))
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()
	return dbcheck.RequireColumn(db, "pending_events", "app_id")
}

// pendingAppID is the app id of a listing event: d on an app, i on a release
// or asset. Other kinds have none.
func pendingAppID(event *nostr.Event) string {
	switch event.Kind {
	case events.KindApp:
		id, _ := events.Find(event.Tags, "d")
		return id
	case events.KindRelease, events.KindAsset:
		id, _ := events.Find(event.Tags, "i")
		return id
	}
	return ""
}

// SavePending stores an event in the pending_events table in an idempotent way.
// It returns true if the event was inserted (i.e. it was not already present), false otherwise.
func (s T) SavePending(ctx context.Context, event *nostr.Event) (bool, error) {
	raw, err := json.Marshal(event)
	if err != nil {
		return false, fmt.Errorf("failed to marshal event: %w", err)
	}
	res, err := s.DB.ExecContext(ctx,
		`INSERT OR IGNORE INTO pending_events (id, kind, raw, received_at, app_id) VALUES (?, ?, ?, ?, ?)`,
		event.ID, event.Kind, string(raw), time.Now().UTC().Unix(), pendingAppID(event),
	)
	if err != nil {
		return false, fmt.Errorf("failed to save pending event: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to get rows affected: %w", err)
	}
	return rows > 0, nil
}

// ListPending returns pending events whose kind is one of kinds.
func (s T) ListPending(ctx context.Context, kinds []int) ([]nostr.Event, error) {
	var all []nostr.Event
	for _, kind := range kinds {
		got, err := s.QueryPending(ctx, kind)
		if err != nil {
			return nil, err
		}
		all = append(all, got...)
	}
	return all, nil
}

// QueryPending returns all pending events of the given kind.
func (s T) QueryPending(ctx context.Context, kind int) ([]nostr.Event, error) {
	return s.queryPending(ctx, `SELECT raw FROM pending_events WHERE kind = ?`, kind)
}

// PendingListing returns the pending apps, releases, and assets for appID, from any pubkey.
func (s T) PendingListing(ctx context.Context, appID string) ([]nostr.Event, error) {
	if appID == "" {
		return nil, nil
	}
	return s.queryPending(ctx, `SELECT raw FROM pending_events WHERE app_id = ? AND kind IN (?, ?, ?)`,
		appID, events.KindApp, events.KindRelease, events.KindAsset)
}

// PendingTagged returns the pending events of kind with a key tag whose
// value is value, ignoring case.
func (s T) PendingTagged(ctx context.Context, kind int, key, value string) ([]nostr.Event, error) {
	return s.queryPending(ctx, `
		SELECT raw FROM pending_events WHERE kind = ? AND EXISTS (
			SELECT 1 FROM json_each(json_extract(raw, '$.tags'))
			WHERE json_extract(value, '$[0]') = ? AND lower(json_extract(value, '$[1]')) = lower(?)
		)`, kind, key, value)
}

func (s T) queryPending(ctx context.Context, query string, args ...any) ([]nostr.Event, error) {
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query pending events: %w", err)
	}
	defer rows.Close()

	var events []nostr.Event
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan pending event: %w", err)
		}
		var event nostr.Event
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return nil, fmt.Errorf("failed to unmarshal pending event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to query pending events: %w", err)
	}
	return events, nil
}

// DeletePending removes a pending event from the pending_events table by its ID.
func (s T) DeletePending(ctx context.Context, ID string) error {
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM pending_events WHERE id = ?`, ID); err != nil {
		return fmt.Errorf("failed to delete pending event: %w", err)
	}
	return nil
}

// DeleteExpiredPending removes all pending events that were received before the given cutoff time.
func (s T) DeleteExpiredPending(ctx context.Context, before time.Time) error {
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM pending_events WHERE received_at < ?`, before.Unix()); err != nil {
		return fmt.Errorf("failed to delete expired pending events: %w", err)
	}
	return nil
}

// ForceDeleteRequest forces a NIP-09 deletion request (kind 5 event), deleting all referenced events, even
// if they have different pubkeys from the deletion request. It returns the number of events deleted.
// This is not a normal NIP-09 deletion, and should only be used by the relay operator.
//
// The event is assumed to have been validated before calling this function.
// Calling DeleteRequest with a non kind-5 event returns [ErrInvalidDeletionRequest].
func (s T) ForceDeleteRequest(ctx context.Context, event *nostr.Event) (int, error) {
	if event.Kind != nostr.KindDeletion {
		return 0, sqlite.ErrInvalidDeletionRequest
	}

	eIDs := findAll(event.Tags, "e")
	aTags := findAll(event.Tags, "a")

	if len(eIDs) == 0 && len(aTags) == 0 {
		return 0, nil
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var deleted int

	// e tags: single batched DELETE with an IN clause.
	if len(eIDs) > 0 {
		query := "DELETE FROM events WHERE id " + inClause(len(eIDs))
		args := make([]any, 0, len(eIDs))
		for _, id := range eIDs {
			args = append(args, id)
		}

		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return 0, fmt.Errorf("failed to delete by e tags: %w", err)
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("failed to check rows affected: %w", err)
		}
		deleted += int(affected)
	}

	// a tags: one DELETE per tag, since each has a distinct kind/pubkey/d combination.
	// Deletes all versions of the addressable event, regardless of creation time.
	for _, a := range aTags {
		ref, err := events.ParseAddressableRef(a)
		if err != nil {
			continue
		}

		query := `DELETE FROM events
			WHERE kind = ? AND pubkey = ?
			AND id IN (SELECT event_id FROM tags WHERE key = 'd' AND value = ?)`
		args := []any{ref.Kind, ref.Pubkey, ref.DTag}

		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return 0, fmt.Errorf("failed to delete by a tag %q: %w", a, err)
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("failed to check rows affected: %w", err)
		}
		deleted += int(affected)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit transaction: %w", err)
	}
	return deleted, nil
}

// findAll returns all values of the given key in the tags.
func findAll(tags nostr.Tags, key string) []string {
	var values []string
	for _, t := range tags {
		if len(t) >= 2 && t[0] == key {
			values = append(values, t[1])
		}
	}
	return values
}

// kindFileMetadata is NIP-94. Flutter 1.1.2 looks a downloaded file up by
// putting its sha256 in search on this kind, and on kind 3063.
const kindFileMetadata = 1063

var sha256Search = regexp.MustCompile(`(?i)^[0-9a-f]{64}$`)

// FileHashSearch reports whether f is a file lookup: search is a sha256 and
// the only kind is 3063 or 1063. The hash is lowercase. The relay answers it
// as #x. Any other search is text search, and only kind 32267 is allowed.
func FileHashSearch(f nostr.Filter) (string, bool) {
	if !sha256Search.MatchString(f.Search) {
		return "", false
	}
	if slices.Equal(f.Kinds, []int{events.KindAsset}) || slices.Equal(f.Kinds, []int{kindFileMetadata}) {
		return strings.ToLower(f.Search), true
	}
	return "", false
}

// Validate returns an error if the given filters are not supported. A text
// search is one filter on kind 32267. A sha256 search on kind 3063 or 1063
// is a file lookup. The store's query builder refuses a search field.
func Validate(filters ...nostr.Filter) error {
	if len(filters) == 0 {
		return errors.New("no filter provided")
	}
	if searchesIn(filters) == 0 {
		return nil
	}
	if len(filters) == 1 {
		if _, ok := FileHashSearch(filters[0]); ok {
			return nil
		}
	}
	if len(filters) > 1 {
		// One search filter per REQ. Several would make the result order ambiguous.
		return fmt.Errorf("%w: there can only be one filter per REQ when using search", ErrUnsupportedREQ)
	}
	if !slices.Equal(filters[0].Kinds, []int{events.KindApp}) {
		return fmt.Errorf("%w: search is only allowed for kind %d", ErrUnsupportedREQ, events.KindApp)
	}
	return nil
}

// queryBuilder rejects a search. Otherwise it builds the same queries as
// [sqlite.DefaultQueryBuilder], except for the tags in [existsTags].
func queryBuilder(filters ...nostr.Filter) ([]sqlite.Query, error) {
	return buildQueries(filters)
}

func buildQueries(filters nostr.Filters) ([]sqlite.Query, error) {
	if err := Validate(filters...); err != nil {
		return nil, err
	}
	if searchesIn(filters) > 0 {
		return nil, fmt.Errorf("%w: text search is not supported", ErrUnsupportedREQ)
	}
	queries := make([]sqlite.Query, 0, len(filters))
	for _, filter := range filters {
		conds, args := filterSQL(filter)
		query := "SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig FROM events AS e"
		if len(conds) > 0 {
			query += " WHERE " + strings.Join(conds, " AND ")
		}
		query += " ORDER BY e.created_at DESC, e.id ASC LIMIT ?"
		args = append(args, filter.Limit)
		queries = append(queries, sqlite.Query{SQL: query, Args: args})
	}
	return queries, nil
}

// existsTags are tag keys whose values match most events of a kind, like f
// (platform) on kind 3063. An IN subquery on such a key lists nearly every
// event before the other conditions apply, so they are checked per row.
var existsTags = map[string]bool{"f": true}

// filterSQL converts a filter to SQL conditions and their arguments.
func filterSQL(filter nostr.Filter) (conds []string, args []any) {
	if len(filter.IDs) > 0 {
		conds = append(conds, "e.id"+inClause(len(filter.IDs)))
		for _, id := range filter.IDs {
			args = append(args, id)
		}
	}
	if len(filter.Kinds) > 0 {
		conds = append(conds, "e.kind"+inClause(len(filter.Kinds)))
		for _, kind := range filter.Kinds {
			args = append(args, kind)
		}
	}
	if len(filter.Authors) > 0 {
		conds = append(conds, "e.pubkey"+inClause(len(filter.Authors)))
		for _, pk := range filter.Authors {
			args = append(args, pk)
		}
	}
	if filter.Until != nil {
		conds = append(conds, "e.created_at <= ?")
		args = append(args, int64(*filter.Until))
	}
	if filter.Since != nil {
		conds = append(conds, "e.created_at >= ?")
		args = append(args, int64(*filter.Since))
	}
	for key, vals := range filter.Tags {
		if len(vals) == 0 || key == "" {
			continue
		}
		if existsTags[key] {
			conds = append(conds, "EXISTS (SELECT 1 FROM tags AS t WHERE t.event_id = e.id AND t.key = ? AND t.value"+inClause(len(vals))+")")
		} else {
			conds = append(conds, "e.id IN (SELECT event_id FROM tags WHERE key = ? AND value"+inClause(len(vals))+")")
		}
		args = append(args, key)
		for _, v := range vals {
			args = append(args, v)
		}
	}
	return conds, args
}

// searchesIn counts the number of filters with a non-empty search term.
func searchesIn(filters nostr.Filters) int {
	count := 0
	for _, filter := range filters {
		if filter.Search != "" {
			count++
		}
	}
	return count
}

// inClause returns " = ?" for a single value or " IN (?, ?, ...)" for multiple values.
func inClause(n int) string {
	if n == 1 {
		return " = ?"
	}
	return " IN (?" + strings.Repeat(",?", n-1) + ")"
}
