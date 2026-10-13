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
	"slices"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	sqlite "github.com/vertex-lab/nostr-sqlite"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/repourl"
)

var ErrUnsupportedREQ = errors.New("unsupported REQ")

//go:embed schema.sql
var schema string

type T struct {
	*sqlite.Store
}

func New(path string) (T, error) {
	store, err := sqlite.New(
		path,
		sqlite.WithAdditionalSchema(schema),
		sqlite.WithQueryBuilder(queryBuilder),
		sqlite.WithBusyTimeout(10*time.Second),
		sqlite.WithCacheSize(256*sqlite.MiB),
		sqlite.WithoutEventPolicy(), // events have been validated by the relay
		sqlite.WithoutQueryPolicy(), // req queries have been validated by the relay
		sqlite.WithoutCountPolicy(), // count queries have been validated by the relay
	)
	if err != nil {
		return T{}, err
	}
	return T{Store: store}, nil
}

// SavePending stores an event in the pending_events table in an idempotent way.
// It returns true if the event was inserted (i.e. it was not already present), false otherwise.
func (s T) SavePending(ctx context.Context, event *nostr.Event) (bool, error) {
	raw, err := json.Marshal(event)
	if err != nil {
		return false, fmt.Errorf("failed to marshal event: %w", err)
	}
	res, err := s.DB.ExecContext(ctx,
		`INSERT OR IGNORE INTO pending_events (id, kind, raw, received_at) VALUES (?, ?, ?, ?)`,
		event.ID, event.Kind, string(raw), time.Now().UTC().Unix(),
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

// Signing returns the DER certificates read from the APK with this sha256,
// oldest first, or the reason it could not be read. ok is false when the APK
// has not been read.
func (s T) Signing(ctx context.Context, blob string) (certs [][]byte, readErr string, ok bool, err error) {
	var raw string
	err = s.DB.QueryRowContext(ctx, `SELECT certs, error FROM apk_signing WHERE blob = ?`, blob).Scan(&raw, &readErr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("failed to read apk signing %s: %w", blob, err)
	}
	if err := json.Unmarshal([]byte(raw), &certs); err != nil {
		return nil, "", false, fmt.Errorf("apk signing %s: %w", blob, err)
	}
	return certs, readErr, true, nil
}

// SaveSigning stores what was read from the APK with this sha256.
func (s T) SaveSigning(ctx context.Context, blob string, certs [][]byte, readErr string) error {
	if certs == nil {
		certs = [][]byte{}
	}
	raw, err := json.Marshal(certs)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO apk_signing (blob, certs, error) VALUES (?, ?, ?)
		ON CONFLICT(blob) DO UPDATE SET certs = excluded.certs, error = excluded.error`, blob, string(raw), readErr)
	if err != nil {
		return fmt.Errorf("failed to save apk signing %s: %w", blob, err)
	}
	return nil
}

// QueryPending returns all pending events of the given kind.
func (s T) QueryPending(ctx context.Context, kind int) ([]nostr.Event, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT raw FROM pending_events WHERE kind = ?`, kind)
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

// Validate returns an error if the given filters are not supported.
// Text search is not supported. A repository URL in the search field is an
// exact match on the repository tag.
func Validate(filters ...nostr.Filter) error {
	if len(filters) == 0 {
		return errors.New("no filter provided")
	}
	if searchesIn(filters) == 0 {
		return nil
	}
	if len(filters) > 1 {
		// One search filter per REQ. Several would make the result order ambiguous.
		return fmt.Errorf("%w: there can only be one filter per REQ when using search", ErrUnsupportedREQ)
	}
	if !slices.Equal(filters[0].Kinds, []int{events.KindApp}) {
		return fmt.Errorf("%w: search is only allowed for kind %d", ErrUnsupportedREQ, events.KindApp)
	}
	if _, ok := repourl.Parse(filters[0].Search); !ok {
		return fmt.Errorf("%w: text search is not supported", ErrUnsupportedREQ)
	}
	return nil
}

// queryBuilder matches a repository URL in the search field against the
// repository tag. Any other search is rejected. Filters without a search
// field use the default query builder.
func queryBuilder(filters ...nostr.Filter) ([]sqlite.Query, error) {
	if err := Validate(filters...); err != nil {
		return nil, err
	}
	if searchesIn(filters) > 0 {
		return searchQuery(filters[0])
	}
	return sqlite.DefaultQueryBuilder(filters...)
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

// repositoryURLQuery performs an exact match on the `repository` tag.
// It tries both the canonical URL and with ".git" appended to handle both storage forms.
func repositoryURLQuery(filter nostr.Filter) ([]sqlite.Query, error) {
	// filter.Search is already canonical (no .git). Match both forms stored in the DB.
	canonical := filter.Search
	withGit := canonical + ".git"

	query := `SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM events e
		JOIN tags t ON t.event_id = e.id
		WHERE e.kind = 32267
		  AND t.key = 'repository'
		  AND (t.value = ? OR t.value = ?)
		LIMIT ?`

	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}

	return []sqlite.Query{{SQL: query, Args: []any{canonical, withGit, limit}}}, nil
}

// searchQuery matches a repository URL against the repository tag.
func searchQuery(f nostr.Filter) ([]sqlite.Query, error) {
	r, ok := repourl.Parse(f.Search)
	if !ok {
		return nil, fmt.Errorf("%w: text search is not supported", ErrUnsupportedREQ)
	}
	f.Search = r.Canonical
	return repositoryURLQuery(f)
}

// inClause returns " = ?" for a single value or " IN (?, ?, ...)" for multiple values.
func inClause(n int) string {
	if n == 1 {
		return " = ?"
	}
	return " IN (?" + strings.Repeat(",?", n-1) + ")"
}
