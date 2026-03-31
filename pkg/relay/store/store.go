// The store package is responsible for storing nostr events in a sqlite database.
// It exposes a [New] function to create a new sqlite store with the given config.
package store

import (
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
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

func New(path string) (*sqlite.Store, error) {
	store, err := sqlite.New(
		path,
		sqlite.WithAdditionalSchema(schema),
		sqlite.WithQueryBuilder(queryBuilder),
		sqlite.WithBusyTimeout(10*time.Second),
		sqlite.WithCacheSize(256*sqlite.MiB),
		sqlite.WithoutEventPolicy(),  // events have been validated by the relay
		sqlite.WithoutFilterPolicy(), // filters have been validated by the relay
	)
	if err != nil {
		return nil, err
	}

	if err := migrate(store); err != nil {
		store.Close()
		return nil, fmt.Errorf("migration failed: %w", err)
	}

	return store, nil
}

//go:embed migrate.sql
var migrateSingleLetterTags string

// migrate applies one-time data migrations tracked by a version table.
func migrate(store *sqlite.Store) error {
	_, err := store.DB.Exec(`CREATE TABLE IF NOT EXISTS migrations (version INTEGER PRIMARY KEY)`)
	if err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	var exists int
	err = store.DB.QueryRow(`SELECT 1 FROM migrations WHERE version = 1`).Scan(&exists)
	if err == nil {
		return nil // already applied
	}

	slog.Info("store: backfilling single-letter tags for existing events (migration 1)")

	if _, err := store.DB.Exec(migrateSingleLetterTags); err != nil {
		return fmt.Errorf("backfill single-letter tags: %w", err)
	}

	if _, err := store.DB.Exec(`INSERT INTO migrations (version) VALUES (1)`); err != nil {
		return fmt.Errorf("record migration 1: %w", err)
	}

	slog.Info("store: migration 1 complete")
	return nil
}

// TODO(transition): remove when all publishers include h tags and legacy events are gone.
const zapstoreCommunityPubkey = events.ZapstoreCommunityPubkey

// queryBuilder handles FTS search for apps when there's exactly one app search filter.
// When the search term is a repository URL (any host, /:user/:repo path), it performs
// an exact match on the `repository` tag instead of FTS. Otherwise, it delegates to
// the default query builder.
func queryBuilder(filters ...nostr.Filter) ([]sqlite.Query, error) {
	if searchesIn(filters) == 0 {
		return tagAwareQueryBuilder(filters...)
	}

	if len(filters) > 1 {
		// We don't support multiple filters when one is using NIP-50 search because the order
		// of the result events will inevitably be ambiguous.
		return nil, fmt.Errorf("%w: there can only be one filter per REQ when using NIP-50 search", ErrUnsupportedREQ)
	}

	search := filters[0]
	if !slices.Equal(search.Kinds, []int{events.KindApp}) {
		return nil, fmt.Errorf("%w: we allow NIP-50 search only for kind %d", ErrUnsupportedREQ, events.KindApp)
	}

	// Repository URL search: exact match on the `repository` tag (no FTS).
	// Accepts any /:user/:repo URL (GitHub, GitLab, Codeberg, etc.) with or
	// without a scheme and with or without trailing path/query.
	if r, ok := repourl.Parse(search.Search); ok {
		search.Search = r.Canonical
		return repositoryURLQuery(search)
	}

	return appSearchQuery(search)
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

// appSearchQuery builds an FTS query for searching apps.
// Results are ordered by BM25 relevance with custom weights.
func appSearchQuery(filter nostr.Filter) ([]sqlite.Query, error) {
	if len(filter.Search) < 3 {
		// Because of the trigram tokenizer, we need at least 3 characters to get a meaningful result.
		return nil, fmt.Errorf("%w: search term must be at least 3 characters", ErrUnsupportedREQ)
	}

	filter.Search = escapeFTS5(filter.Search)
	conditions, args := appSearchSql(filter)

	query := `SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
		FROM events e
		JOIN apps_fts fts ON e.id = fts.id
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY bm25(apps_fts, 0, 20, 5, 1)
		LIMIT ?`

	args = append(args, filter.Limit)
	return []sqlite.Query{{SQL: query, Args: args}}, nil
}

// appSearchSql converts a nostr.Filter into SQL conditions and arguments.
// Tags are filtered using subqueries to avoid JOIN and GROUP BY,
// which would break bm25() ranking.
func appSearchSql(filter nostr.Filter) (conditions []string, args []any) {
	conditions = []string{"apps_fts MATCH ?"}
	args = []any{filter.Search}

	if len(filter.IDs) > 0 {
		conditions = append(conditions, "e.id"+inClause(len(filter.IDs)))
		for _, id := range filter.IDs {
			args = append(args, id)
		}
	}

	if len(filter.Authors) > 0 {
		conditions = append(conditions, "e.pubkey"+inClause(len(filter.Authors)))
		for _, pk := range filter.Authors {
			args = append(args, pk)
		}
	}

	if filter.Since != nil {
		conditions = append(conditions, "e.created_at >= ?")
		args = append(args, filter.Since.Time().Unix())
	}

	if filter.Until != nil {
		conditions = append(conditions, "e.created_at <= ?")
		args = append(args, filter.Until.Time().Unix())
	}

	for key, vals := range filter.Tags {
		if len(vals) == 0 {
			continue
		}
		conditions = append(conditions,
			"EXISTS (SELECT 1 FROM tags WHERE event_id = e.id AND key = ? AND value"+inClause(len(vals))+")")
		args = append(args, key)
		for _, v := range vals {
			args = append(args, v)
		}
	}
	return conditions, args
}

// escapeFTS5 prepares a search term for SQLite FTS5
func escapeFTS5(term string) string {
	term = strings.ReplaceAll(term, `"`, `""`)
	return `"` + term + `"`
}

// inClause returns " = ?" for a single value or " IN (?, ?, ...)" for multiple values.
func inClause(n int) string {
	if n == 1 {
		return " = ?"
	}
	return " IN (?" + strings.Repeat(",?", n-1) + ")"
}

// tagAwareQueryBuilder routes each filter through the appropriate builder.
// Community app set filters (kind 30267 with h=community_key) get special
// handling so the SQL LIMIT applies after private-event filtering.
// TODO(transition): collapse back to DefaultQueryBuilder when h-tag migration is complete.
func tagAwareQueryBuilder(filters ...nostr.Filter) ([]sqlite.Query, error) {
	queries := make([]sqlite.Query, 0, len(filters))
	for _, f := range filters {
		if q, ok := communityAppSetQuery(f); ok {
			queries = append(queries, q)
		} else {
			qs, err := sqlite.DefaultQueryBuilder(f)
			if err != nil {
				return nil, err
			}
			queries = append(queries, qs...)
		}
	}
	return queries, nil
}

// communityAppSetQuery detects kind 30267 filters with h=zapstoreCommunityPubkey
// and generates a single SQL query that correctly handles the h-tag transition:
//
//   - Events WITH h=community_key are matched directly (new, properly tagged events).
//   - Events WITHOUT any h tag AND with empty content are matched as legacy public events.
//
// By pushing both the community-membership and public-only checks into SQL, the
// LIMIT clause applies to the already-filtered result set. This avoids the bug where
// SQL LIMIT fetched N events and Go post-filtering removed private ones, returning
// far fewer than N results.
//
// TODO(transition): remove when all publishers include h tags.
func communityAppSetQuery(f nostr.Filter) (sqlite.Query, bool) {
	hVals, hasH := f.Tags["h"]
	if !hasH || !slices.Contains(hVals, zapstoreCommunityPubkey) {
		return sqlite.Query{}, false
	}
	if !slices.Contains(f.Kinds, events.KindAppSet) {
		return sqlite.Query{}, false
	}

	var conds []string
	var args []any

	if len(f.Kinds) == 1 {
		conds = append(conds, "e.kind = ?")
		args = append(args, f.Kinds[0])
	} else {
		conds = append(conds, "e.kind"+inClause(len(f.Kinds)))
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}

	if len(f.IDs) > 0 {
		conds = append(conds, "e.id"+inClause(len(f.IDs)))
		for _, id := range f.IDs {
			args = append(args, id)
		}
	}

	if len(f.Authors) > 0 {
		conds = append(conds, "e.pubkey"+inClause(len(f.Authors)))
		for _, pk := range f.Authors {
			args = append(args, pk)
		}
	}

	if f.Since != nil {
		conds = append(conds, "e.created_at >= ?")
		args = append(args, int64(*f.Since))
	}
	if f.Until != nil {
		conds = append(conds, "e.created_at <= ?")
		args = append(args, int64(*f.Until))
	}

	for key, vals := range f.Tags {
		if key == "h" || len(vals) == 0 {
			continue
		}
		conds = append(conds, "e.id IN (SELECT event_id FROM tags WHERE key = ? AND value"+inClause(len(vals))+")")
		args = append(args, key)
		for _, v := range vals {
			args = append(args, v)
		}
	}

	// h-tag transition: events with h=community OR legacy events (no h tag, public content)
	conds = append(conds,
		"(e.id IN (SELECT event_id FROM tags WHERE key = 'h' AND value = ?)"+
			" OR (NOT EXISTS (SELECT 1 FROM tags WHERE event_id = e.id AND key = 'h') AND e.content = ''))")
	args = append(args, zapstoreCommunityPubkey)

	query := "SELECT e.* FROM events AS e WHERE " + strings.Join(conds, " AND ") +
		" ORDER BY e.created_at DESC, e.id ASC LIMIT ?"
	args = append(args, f.Limit)

	return sqlite.Query{SQL: query, Args: args}, true
}
