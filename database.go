package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jmoiron/sqlx"
	"github.com/jmoiron/sqlx/reflectx"
	_ "github.com/mattn/go-sqlite3"
	"github.com/nbd-wtf/go-nostr"
)

type SQLite3Backend struct {
	sync.Mutex
	*sqlx.DB
	DatabaseURL       string
	QueryLimit        int
	QueryIDsLimit     int
	QueryAuthorsLimit int
	QueryKindsLimit   int
	QueryTagsLimit    int
}

func (b *SQLite3Backend) Close() {
	b.DB.Close()
}

var ErrDupEvent = errors.New("duplicate: event already exists")

const (
	queryLimit        = 100
	queryIDsLimit     = 500
	queryAuthorsLimit = 500
	queryKindsLimit   = 10
	queryTagsLimit    = 10
)

var ddls = []string{
	`CREATE TABLE IF NOT EXISTS event (
       id text NOT NULL,
       pubkey text NOT NULL,
       created_at integer NOT NULL,
       kind integer NOT NULL,
       tags jsonb NOT NULL,
       content text NOT NULL,
       sig text NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS whitelist (
       pubkey text NOT NULL,
       level integer NOT NULL);`,
	`CREATE TABLE IF NOT EXISTS logs (
       log text NOT NULL);`,

	// FTS5: index “id” + “tags” so that we can search inside the JSON quickly
	`CREATE VIRTUAL TABLE IF NOT EXISTS event_fts USING fts5(
        id,
        tags,
        tokenize = 'unicode61'  -- default tokenizer
    );`,
	`CREATE UNIQUE INDEX IF NOT EXISTS ididx ON event(id)`,
	`CREATE INDEX IF NOT EXISTS pubkeyprefix ON event(pubkey)`,
	`CREATE INDEX IF NOT EXISTS timeidx ON event(created_at DESC)`,
	`CREATE INDEX IF NOT EXISTS kindidx ON event(kind)`,
	`CREATE INDEX IF NOT EXISTS kindtimeidx ON event(kind,created_at DESC)`,
}

func (b *SQLite3Backend) Init() error {
	db, err := sqlx.Connect("sqlite3", b.DatabaseURL)
	if err != nil {
		return err
	}

	db.Mapper = reflectx.NewMapperFunc("json", sqlx.NameMapper)
	b.DB = db

	for _, ddl := range ddls {
		_, err = b.DB.Exec(ddl)
		if err != nil {
			return err
		}
	}

	if b.QueryLimit == 0 {
		b.QueryLimit = queryLimit
	}
	if b.QueryIDsLimit == 0 {
		b.QueryIDsLimit = queryIDsLimit
	}
	if b.QueryAuthorsLimit == 0 {
		b.QueryAuthorsLimit = queryAuthorsLimit
	}
	if b.QueryKindsLimit == 0 {
		b.QueryKindsLimit = queryKindsLimit
	}
	if b.QueryTagsLimit == 0 {
		b.QueryTagsLimit = queryTagsLimit
	}
	return nil
}

func (b SQLite3Backend) DeleteEvent(ctx context.Context, evt *nostr.Event) error {
	if _, err := b.DB.ExecContext(ctx, `DELETE FROM event WHERE id = ?`, evt.ID); err != nil {
		return err
	}
	if _, err := b.DB.ExecContext(ctx, `DELETE FROM event_fts WHERE id = ?`, evt.ID); err != nil {
		return err
	}
	return nil
}

func (b *SQLite3Backend) SaveEvent(ctx context.Context, evt *nostr.Event) error {
	tagsj, _ := json.Marshal(evt.Tags)
	res, err := b.DB.ExecContext(ctx, `
        INSERT OR IGNORE INTO event (id, pubkey, created_at, kind, tags, content, sig)
        VALUES (?, ?, ?, ?, ?, ?, ?);
    `, evt.ID, evt.PubKey, evt.CreatedAt, evt.Kind, tagsj, evt.Content, evt.Sig)
	if err != nil {
		return err
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected == 0 {
		return ErrDupEvent
	}

	ftstags := make(nostr.Tags, 0)
	for _, t := range evt.Tags {
		if len(t) < 2 {
			continue
		}

		if t[0] == "name" || t[0] == "url" {
			ftstags = append(ftstags, t)
		}
	}

	tagsjfts, _ := json.Marshal(ftstags)

	if _, err := b.DB.ExecContext(ctx, `
        INSERT INTO event_fts (id, tags) VALUES (?, ?);
    `, evt.ID, string(tagsjfts)); err != nil {
		return err
	}

	return nil
}

func (b *SQLite3Backend) Savelog(ctx context.Context, log string) error {
	_, err := b.DB.ExecContext(ctx, `
        INSERT OR IGNORE INTO logs (log)
        VALUES ($1)
    `, log)
	if err != nil {
		return err
	}

	return nil
}

func (b *SQLite3Backend) ReplaceEvent(ctx context.Context, evt *nostr.Event) error {
	b.Lock()
	defer b.Unlock()

	filter := nostr.Filter{Limit: 1, Kinds: []int{evt.Kind}, Authors: []string{evt.PubKey}}
	if nostr.IsAddressableKind(evt.Kind) {
		filter.Tags = nostr.TagMap{"d": []string{evt.Tags.GetD()}}
	}

	ch, err := b.QueryEvents(ctx, filter)
	if err != nil {
		return fmt.Errorf("failed to query before replacing: %w", err)
	}

	shouldStore := true
	for previous := range ch {
		if IsOlder(previous, evt) {
			if err := b.DeleteEvent(ctx, previous); err != nil {
				return fmt.Errorf("failed to delete event for replacing: %w", err)
			}
		} else {
			shouldStore = false
		}
	}

	if shouldStore {
		if err := b.SaveEvent(ctx, evt); err != nil && err != ErrDupEvent {
			return fmt.Errorf("failed to save: %w", err)
		}
	}

	return nil
}

func IsOlder(previous, next *nostr.Event) bool {
	return previous.CreatedAt < next.CreatedAt ||
		(previous.CreatedAt == next.CreatedAt && previous.ID > next.ID)
}

func (b SQLite3Backend) QueryEvents(ctx context.Context, filter nostr.Filter) (ch chan *nostr.Event, err error) {
	query, params, err := b.queryEventsSql(filter)
	if err != nil {
		return nil, err
	}

	rows, err := b.DB.QueryContext(ctx, query, params...)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("failed to fetch events using query %q: %w", query, err)
	}

	ch = make(chan *nostr.Event)
	go func() {
		defer rows.Close()
		defer close(ch)
		for rows.Next() {
			var evt nostr.Event
			var timestamp int64
			err := rows.Scan(&evt.ID, &evt.PubKey, &timestamp,
				&evt.Kind, &evt.Tags, &evt.Content, &evt.Sig)
			if err != nil {
				return
			}
			evt.CreatedAt = nostr.Timestamp(timestamp)
			select {
			case ch <- &evt:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

var (
	TooManyIDs       = errors.New("too many ids")
	TooManyAuthors   = errors.New("too many authors")
	TooManyKinds     = errors.New("too many kinds")
	TooManyTagValues = errors.New("too many tag values")
	EmptyTagSet      = errors.New("empty tag set")
)

func makePlaceHolders(n int) string {
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

func (b SQLite3Backend) queryEventsSql(filter nostr.Filter) (string, []any, error) {
	conditions := make([]string, 0, 10)
	params := make([]any, 0, 20)

	joinFTS := false

	if len(filter.IDs) > 0 {
		if len(filter.IDs) > b.QueryIDsLimit {
			return "", nil, TooManyIDs
		}
		ph := makePlaceHolders(len(filter.IDs))
		conditions = append(conditions, fmt.Sprintf("e.id IN (%s)", ph))
		for _, v := range filter.IDs {
			params = append(params, v)
		}
	}

	if len(filter.Authors) > 0 {
		if len(filter.Authors) > b.QueryAuthorsLimit {
			return "", nil, TooManyAuthors
		}
		ph := makePlaceHolders(len(filter.Authors))
		conditions = append(conditions, fmt.Sprintf("e.pubkey IN (%s)", ph))
		for _, v := range filter.Authors {
			params = append(params, v)
		}
	}

	if len(filter.Kinds) > 0 {
		if len(filter.Kinds) > b.QueryKindsLimit {
			return "", nil, TooManyKinds
		}
		ph := makePlaceHolders(len(filter.Kinds))
		conditions = append(conditions, fmt.Sprintf("e.kind IN (%s)", ph))
		for _, v := range filter.Kinds {
			params = append(params, v)
		}
	}

	totalTags := 0
	for _, values := range filter.Tags {
		if len(values) == 0 {
			return "", nil, EmptyTagSet
		}
		orTag := make([]string, len(values))
		for i, tv := range values {
			orTag[i] = "e.tags LIKE ? ESCAPE '\\'"
			params = append(params, "%"+strings.ReplaceAll(tv, "%", "\\%")+"%")
		}
		conditions = append(conditions, "("+strings.Join(orTag, " OR ")+")")
		totalTags += len(values)
		if totalTags > b.QueryTagsLimit {
			return "", nil, TooManyTagValues
		}
	}

	if filter.Since != nil {
		conditions = append(conditions, "e.created_at >= ?")
		params = append(params, filter.Since)
	}
	if filter.Until != nil {
		conditions = append(conditions, "e.created_at <= ?")
		params = append(params, filter.Until)
	}

	if filter.Search != "" {
		joinFTS = true
		conditions = append(conditions, "event_fts MATCH ?")
		params = append(params, filter.Search)
	}

	// Fallback if no WHERE clause
	if len(conditions) == 0 {
		conditions = append(conditions, "1=1")
	}

	limit := b.QueryLimit
	if filter.Limit >= 1 && filter.Limit <= b.QueryLimit {
		limit = filter.Limit
	}
	params = append(params, limit)

	var query string
	if joinFTS {
		query = fmt.Sprintf(`
                SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
                  FROM event AS e
                  JOIN event_fts ON event_fts.id = e.id
                 WHERE %s
              ORDER BY e.created_at DESC, e.id
                 LIMIT ?
            `, strings.Join(conditions, " AND "))
	} else {
		query = fmt.Sprintf(`
                SELECT e.id, e.pubkey, e.created_at, e.kind, e.tags, e.content, e.sig
                  FROM event AS e
                 WHERE %s
              ORDER BY e.created_at DESC, e.id
                 LIMIT ?
            `, strings.Join(conditions, " AND "))
	}

	return query, params, nil
}

func (b *SQLite3Backend) GetWhitelistLevel(ctx context.Context, pubkey string) (int, error) {
	var level int
	err := b.DB.GetContext(ctx, &level, `
        SELECT level FROM whitelist WHERE pubkey = ?
    `, pubkey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return level, nil
}
